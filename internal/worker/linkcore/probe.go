package linkcore

import (
	"context"
	"sync"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Prober runs probes: short requests answered outside the job slots
// (*probe.Executor). Probe never fails; the outcome is in the result.
type Prober interface {
	Probe(ctx context.Context, p *syncrv1.RunProbe) *syncrv1.ProbeResult
}

// Option configures a Core.
type Option func(*Core)

// WithProber makes the Core run probes with p. Without it, every probe is
// answered FAILED with UNSUPPORTED_KIND.
func WithProber(p Prober) Option {
	return func(c *Core) { c.probes.prober = p }
}

// Retention of probe state (active mode).
const (
	// ProbeResultRetention is how long a finished probe's result waits for
	// delivery before it is dropped: the controller has long stopped
	// waiting by then (its own bound is the probe timeout, at most 90s,
	// plus a grace period).
	ProbeResultRetention = 2 * time.Minute
	// ProbeDedupeRetention is how long a probe ID is remembered after the
	// probe started, so a redelivered run_probe never runs twice: three
	// times the longest probe.
	ProbeDedupeRetention = 270 * time.Second
)

// probeState is one active-mode probe.
type probeState struct {
	started time.Time
	cancel  context.CancelFunc
	// result is set when the probe finished and cleared once delivered.
	result   *syncrv1.ProbeResult
	finished time.Time
}

// probes is the probe state of a Core.
type probes struct {
	prober Prober

	mu     sync.Mutex
	byID   map[string]*probeState
	notify chan struct{}
	wg     sync.WaitGroup
	// base ends every probe when the Core is closed.
	base   context.Context //nolint:containedctx // the lifetime of the Core's probes, not a request context
	stop   context.CancelFunc
	closed bool
}

func (p *probes) init() {
	p.byID = make(map[string]*probeState)
	p.notify = make(chan struct{}, 1)
	p.base, p.stop = context.WithCancel(context.Background())
}

// unsupported is the result of a probe on a worker without a Prober.
func unsupported(p *syncrv1.RunProbe) *syncrv1.ProbeResult {
	return &syncrv1.ProbeResult{
		ProbeId: p.GetProbeId(),
		Status:  syncrv1.ProbeStatus_PROBE_STATUS_FAILED,
		Error: &syncrv1.JobError{
			Code:    syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND,
			Message: "this worker cannot run probes",
		},
	}
}

func (p *probes) run(ctx context.Context, rp *syncrv1.RunProbe) *syncrv1.ProbeResult {
	if p.prober == nil {
		return unsupported(rp)
	}
	res := p.prober.Probe(ctx, rp)
	if res == nil {
		res = unsupported(rp)
	}
	res.ProbeId = rp.GetProbeId()
	return res
}

// purge forgets state past its retention. The caller holds mu.
func (p *probes) purge(now time.Time) {
	for id, st := range p.byID {
		if st.result != nil && now.Sub(st.finished) >= ProbeResultRetention {
			st.result = nil
		}
		if st.finished.IsZero() {
			continue // still running
		}
		if st.result == nil && now.Sub(st.started) >= ProbeDedupeRetention {
			delete(p.byID, id)
		}
	}
}

// start runs rp in the background unless its ID is already known.
func (p *probes) start(rp *syncrv1.RunProbe) {
	id := rp.GetProbeId()
	if id == "" {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge(now)
	if _, dup := p.byID[id]; dup || p.closed {
		return
	}
	ctx, cancel := context.WithCancel(p.base)
	st := &probeState{started: now, cancel: cancel}
	p.byID[id] = st
	p.wg.Go(func() {
		defer cancel()
		res := p.run(ctx, rp)
		p.mu.Lock()
		st.result, st.finished = res, time.Now()
		p.mu.Unlock()
		select {
		case p.notify <- struct{}{}:
		default:
		}
	})
}

// cancel stops a running probe; unknown and finished ones are ignored.
func (p *probes) cancel(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.byID[id]; ok && st.finished.IsZero() {
		st.cancel()
	}
}

// Epoch returns the worker process epoch, which accompanies a probe result
// (CompleteProbe).
func (c *Core) Epoch() string { return c.agent.Epoch() }

// ProbeNotify signals that a probe finished and its result waits for
// delivery (active mode). The channel has a buffer of one: a signal may
// stand for several results.
func (c *Core) ProbeNotify() <-chan struct{} { return c.probes.notify }

// PendingProbeResults returns the results of finished probes that were not
// delivered yet, oldest first (active mode). A result is dropped after
// ProbeResultRetention.
func (c *Core) PendingProbeResults() []*syncrv1.ProbeResult {
	p := &c.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge(time.Now())
	var pending []*probeState
	for _, st := range p.byID {
		if st.result != nil {
			pending = append(pending, st)
		}
	}
	// Oldest first, so a slow link delivers them in the order they ended.
	for i := 1; i < len(pending); i++ {
		for j := i; j > 0 && pending[j].finished.Before(pending[j-1].finished); j-- {
			pending[j], pending[j-1] = pending[j-1], pending[j]
		}
	}
	out := make([]*syncrv1.ProbeResult, len(pending))
	for i, st := range pending {
		out[i] = st.result
	}
	return out
}

// ProbeDelivered forgets the result of probeID: the controller has it, or
// answered that it no longer wants it.
func (c *Core) ProbeDelivered(probeID string) {
	p := &c.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.byID[probeID]; ok {
		st.result = nil
	}
}

// HandleProbe runs a passive-mode probe and returns its result. The probe
// ends when ctx does (the controller's call) or the Core is closed.
func (c *Core) HandleProbe(ctx context.Context, req *syncrv1.ProbeRequest) *syncrv1.ProbeResponse {
	p := &c.probes
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(p.base, cancel)()
	return &syncrv1.ProbeResponse{Result: p.run(ctx, req.GetProbe())}
}

// Close cancels the probes in flight and waits for them to end. Probes that
// arrive later are refused (active) or cancelled at once (passive). It is
// part of the worker's shutdown, before the agent's.
func (c *Core) Close() {
	p := &c.probes
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.stop()
	p.wg.Wait()
}

// takeProbeCommands handles the probe commands among cmds and returns their
// acks and the remaining commands, for the agent.
func (c *Core) takeProbeCommands(cmds []*syncrv1.Command) (acks []*syncrv1.CommandAck, rest []*syncrv1.Command) {
	rest = make([]*syncrv1.Command, 0, len(cmds))
	for _, cmd := range cmds {
		switch {
		case cmd.GetRunProbe() != nil:
			c.probes.start(cmd.GetRunProbe())
		case cmd.GetCancelProbe() != nil:
			c.probes.cancel(cmd.GetCancelProbe().GetProbeId())
		default:
			rest = append(rest, cmd)
			continue
		}
		if id := cmd.GetId(); id != "" {
			acks = append(acks, &syncrv1.CommandAck{CommandId: id})
		}
	}
	return acks, rest
}
