package linkcore

import (
	"context"
	"sync"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
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

// Retention of probe state (active mode), in multiples of the longest
// probe (link.MaxProbeTimeout).
const (
	// ProbeResultRetention is how long a finished probe's result waits for
	// delivery before it is dropped: twice the longest probe. The
	// controller has long stopped waiting by then (its own bound is the
	// probe's timeout plus link.ProbeGrace).
	ProbeResultRetention = 2 * link.MaxProbeTimeout
	// ProbeDedupeRetention is how long a probe ID is remembered after the
	// probe started, or after a cancel_probe for an ID the worker did not
	// know, so a redelivered or late run_probe never runs twice or after
	// its cancel: three times the longest probe.
	ProbeDedupeRetention = 3 * link.MaxProbeTimeout
)

// probeState is one active-mode probe, or the tombstone of a probe that
// was cancelled before its run_probe arrived (cancel is nil, started and
// finished are the time of the cancel).
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

// start runs rp in the background unless its ID is already known: running,
// finished or cancelled (also cancelled before it arrived).
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

// cancel stops a running probe; a finished one is left alone. An unknown
// ID is remembered for ProbeDedupeRetention, so its run_probe never starts
// if it arrives later: the controller cancels a probe it gave up on as
// soon as its run_probe may have been sent, and the cancel can overtake it
// (a lost Poll response, a redelivery).
func (p *probes) cancel(id string) {
	if id == "" {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge(now)
	st, ok := p.byID[id]
	switch {
	case !ok:
		p.byID[id] = &probeState{started: now, finished: now}
	case st.finished.IsZero():
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
// acks and the remaining commands, for the agent. The cancel_probe
// commands are applied first, so a run_probe and its cancel in one batch
// never start the probe, whatever their order.
func (c *Core) takeProbeCommands(cmds []*syncrv1.Command) (acks []*syncrv1.CommandAck, rest []*syncrv1.Command) {
	for _, cmd := range cmds {
		if cp := cmd.GetCancelProbe(); cp != nil {
			c.probes.cancel(cp.GetProbeId())
		}
	}
	rest = make([]*syncrv1.Command, 0, len(cmds))
	for _, cmd := range cmds {
		switch {
		case cmd.GetRunProbe() != nil:
			c.probes.start(cmd.GetRunProbe())
		case cmd.GetCancelProbe() != nil:
			// Applied above.
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
