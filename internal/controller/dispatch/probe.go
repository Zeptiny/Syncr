package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Probe command ID prefixes: "probe:<probe id>" carries a run_probe,
// "probe-cancel:<probe id>" a cancel_probe.
const (
	probePrefix       = "probe:"
	probeCancelPrefix = "probe-cancel:"
)

const (
	// maxProbeCommands bounds the run_probe commands of one derivation.
	maxProbeCommands = 16
	// maxProbeCancels bounds the cancel_probe commands kept per worker; the
	// oldest are dropped (their probes end by their own timeout).
	maxProbeCancels = 64
	// defaultProbeTimeout is the timeout of a RunProbe that sets none (the
	// workers' default).
	defaultProbeTimeout = 30 * time.Second
)

// ErrProbeUnsupported is what a PassiveClient's Probe returns (wrapped)
// when the worker answered Unimplemented: it has no probe support. It
// matches domain.ErrFailedPrecondition.
var ErrProbeUnsupported = fmt.Errorf("the worker does not support probes; upgrade it: %w", domain.ErrFailedPrecondition)

// ProbeConfig tunes the ProbeBroker. Zero fields take the
// DefaultProbeConfig values.
type ProbeConfig struct {
	// MaxInFlightPerWorker bounds the probes waiting for one worker; more
	// fail with domain.ErrUnavailable at once.
	MaxInFlightPerWorker int
	// MaxTimeout caps a probe's RunProbe.timeout (workers cap it too).
	MaxTimeout time.Duration
	// Grace is added to a probe's timeout for the controller's own wait
	// (delivery, the worker's answer).
	Grace time.Duration
}

// DefaultProbeConfig returns the default configuration: 8 in flight per
// worker, a 90 s cap and 5 s of grace.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{MaxInFlightPerWorker: 8, MaxTimeout: 90 * time.Second, Grace: 5 * time.Second}
}

func (c ProbeConfig) withDefaults() ProbeConfig {
	def := DefaultProbeConfig()
	c.MaxInFlightPerWorker = cmp.Or(c.MaxInFlightPerWorker, def.MaxInFlightPerWorker)
	c.MaxTimeout = cmp.Or(c.MaxTimeout, def.MaxTimeout)
	c.Grace = cmp.Or(c.Grace, def.Grace)
	return c
}

// ProbeBrokerDeps are the ProbeBroker's collaborators. All are required
// except Logger.
type ProbeBrokerDeps struct {
	Workers Workers
	// Hub must be the dispatcher's Hub: a queued run_probe wakes the
	// worker's long poll.
	Hub   *Hub
	Clock Clock
	// Logger defaults to discarding. Probe specs are never logged.
	Logger *slog.Logger
}

// ProbeBroker runs probes on workers (M5 design §6.4). It breaks the wiring
// cycle between the dispatcher and the controller's probe runner: the
// runner calls Probe, the dispatcher (Deps.Probes) hands the broker's
// pending probes to active workers as Command.run_probe and their results
// (Service.CompleteProbe) back, and passive loops register their clients
// so Probe can call WorkerLinkService.Probe directly. Probes live in
// memory only; a controller restart fails the waiting callers with
// domain.ErrUnavailable. Safe for concurrent use.
//
// Active workers: a call waits in the pending table until its caller gives
// up or the result arrives. Until the worker acks its run_probe
// ("probe:<id>") the command is derived again on every poll (workers
// deduplicate by probe ID); a caller that goes away after the ack leaves a
// cancel_probe ("probe-cancel:<id>") behind, sent until it is acked. When
// the worker's epoch changes, the calls its dead process had accepted fail
// ("the worker restarted") and the others are sent to the new process.
type ProbeBroker struct {
	workers Workers
	hub     *Hub
	clock   Clock
	log     *slog.Logger
	cfg     ProbeConfig

	mu sync.Mutex
	// calls are the waiting probes of active workers, in arrival order.
	calls map[domain.WorkerID][]*probeCall
	// cancels are the probe IDs each active worker must still be told to
	// stop.
	cancels map[domain.WorkerID][]string
	// passive holds the registered client of each running passive loop
	// and inFlight the probes being called through them.
	passive  map[domain.WorkerID]*passiveRegistration
	inFlight map[domain.WorkerID]int
}

// probeCall is one waiting probe of an active worker. The broker's mutex
// guards delivered; done receives exactly one outcome, when the call
// leaves the table for a reason other than its caller giving up.
type probeCall struct {
	id        string
	run       *syncrv1.RunProbe
	delivered bool
	done      chan probeOutcome
}

type probeOutcome struct {
	result *syncrv1.ProbeResult
	err    error
}

// passiveRegistration identifies one registration of a passive client, so
// a loop that ends never removes its successor's client.
type passiveRegistration struct {
	client PassiveClient
}

// NewProbeBroker returns a ProbeBroker.
func NewProbeBroker(d ProbeBrokerDeps, cfg ProbeConfig) (*ProbeBroker, error) {
	if d.Workers == nil || d.Hub == nil || d.Clock == nil {
		return nil, errors.New("dispatch: probe broker: missing dependency")
	}
	cfg = cfg.withDefaults()
	if cfg.MaxInFlightPerWorker < 1 || cfg.MaxTimeout <= 0 || cfg.Grace < 0 {
		return nil, fmt.Errorf("dispatch: probe broker: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ProbeBroker{
		workers: d.Workers, hub: d.Hub, clock: d.Clock, log: log, cfg: cfg,
		calls: make(map[domain.WorkerID][]*probeCall), cancels: make(map[domain.WorkerID][]string),
		passive: make(map[domain.WorkerID]*passiveRegistration), inFlight: make(map[domain.WorkerID]int),
	}, nil
}

// MaxTimeout returns the cap on a probe's timeout (ProbeConfig.MaxTimeout).
func (b *ProbeBroker) MaxTimeout() time.Duration { return b.cfg.MaxTimeout }

// Probe runs p on the worker and returns its result: in active mode as a
// Command.run_probe delivered by the worker's long poll and answered with
// CompleteProbe, in passive mode by calling the worker's Probe RPC with
// ctx and the deadline p.timeout + Grace. A result with status FAILED or
// CANCELLED is a result, not an error. p.timeout is capped at MaxTimeout;
// unset means 30 s.
//
// Errors: domain.ErrInvalidArgument (no probe ID or operation);
// domain.ErrNotFound (no such worker, or removed);
// domain.ErrUnavailable (offline or pending, passive link not running, no
// answer in time, the worker restarted, a transport failure, more than
// MaxInFlightPerWorker); domain.ErrFailedPrecondition (the worker lacks
// link.FeatureProbe or the op: "worker %q (version %s) cannot run %s;
// upgrade it", or a passive worker answered Unimplemented); ctx's errors.
// No error names the spec's contents.
func (b *ProbeBroker) Probe(ctx context.Context, workerID domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	op := link.ProbeOpOf(p.GetSpec())
	if p.GetProbeId() == "" || op == "" {
		return nil, fmt.Errorf("probe worker %s: %w: a probe needs an ID and an operation", workerID, domain.ErrInvalidArgument)
	}
	w, err := b.workers.Get(ctx, workerID)
	if err != nil {
		return nil, fmt.Errorf("probe worker %s: %w", workerID, err)
	}
	if w.Removed() {
		return nil, fmt.Errorf("probe worker %s: %w", workerID, domain.ErrNotFound)
	}
	switch b.workers.Status(w) {
	case domain.WorkerOnline:
	case domain.WorkerPending:
		return nil, fmt.Errorf("%w: worker %q has never connected", domain.ErrUnavailable, w.Name)
	default:
		return nil, fmt.Errorf("%w: worker %q is offline", domain.ErrUnavailable, w.Name)
	}
	if !w.Info.HasFeatures(link.FeatureProbe) || !slices.Contains(w.Info.ProbeOps, op) {
		return nil, fmt.Errorf("%w: worker %q (version %s) cannot run %s; upgrade it",
			domain.ErrFailedPrecondition, w.Name, cmp.Or(w.Info.Version, "unknown"), op)
	}
	timeout := defaultProbeTimeout
	if p.GetTimeout() != nil && p.GetTimeout().AsDuration() > 0 {
		timeout = p.GetTimeout().AsDuration()
	}
	timeout = min(timeout, b.cfg.MaxTimeout)
	run, ok := proto.Clone(p).(*syncrv1.RunProbe)
	if !ok {
		return nil, fmt.Errorf("probe worker %s: %w: cannot copy the probe", workerID, domain.ErrInvalidArgument)
	}
	run.Timeout = durationpb.New(timeout)
	if w.Mode == domain.WorkerPassive {
		return b.probePassive(ctx, w, run, timeout+b.cfg.Grace)
	}
	return b.probeActive(ctx, w, run, timeout+b.cfg.Grace)
}

// probeActive queues run for the worker's next poll and waits for its
// result.
func (b *ProbeBroker) probeActive(ctx context.Context, w workers.Worker, run *syncrv1.RunProbe, wait time.Duration) (*syncrv1.ProbeResult, error) {
	call := &probeCall{id: run.GetProbeId(), run: run, done: make(chan probeOutcome, 1)}
	b.mu.Lock()
	switch {
	case len(b.calls[w.ID]) >= b.cfg.MaxInFlightPerWorker:
		b.mu.Unlock()
		return nil, b.busy(w)
	case slices.ContainsFunc(b.calls[w.ID], func(c *probeCall) bool { return c.id == call.id }):
		b.mu.Unlock()
		return nil, fmt.Errorf("probe worker %s: %w: probe %s is already running", w.ID, domain.ErrAlreadyExists, call.id)
	}
	b.calls[w.ID] = append(b.calls[w.ID], call)
	b.mu.Unlock()
	b.hub.Wake(w.ID)

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case o := <-call.done:
		return o.result, o.err
	case <-ctx.Done():
		if o, ok := b.abandon(w.ID, call); ok {
			return o.result, o.err
		}
		return nil, fmt.Errorf("probe worker %s: %w", w.ID, ctx.Err())
	case <-t.C:
		if o, ok := b.abandon(w.ID, call); ok {
			return o.result, o.err
		}
		return nil, fmt.Errorf("%w: worker %q did not answer in time", domain.ErrUnavailable, w.Name)
	}
}

// abandon removes a call whose caller gives up. If the worker accepted the
// probe it is told to stop it. A call that ended meanwhile is returned
// with ok: its outcome stands.
func (b *ProbeBroker) abandon(workerID domain.WorkerID, call *probeCall) (probeOutcome, bool) {
	b.mu.Lock()
	if !b.removeLocked(workerID, call) {
		b.mu.Unlock()
		return <-call.done, true // ended concurrently; the outcome is buffered
	}
	wake := call.delivered
	if wake {
		b.cancels[workerID] = append(b.cancels[workerID], call.id)
		if n := len(b.cancels[workerID]); n > maxProbeCancels {
			b.cancels[workerID] = b.cancels[workerID][n-maxProbeCancels:]
		}
	}
	b.mu.Unlock()
	if wake {
		b.hub.Wake(workerID)
	}
	return probeOutcome{}, false
}

// removeLocked takes call out of the worker's table; false if it is not in
// it any more.
func (b *ProbeBroker) removeLocked(workerID domain.WorkerID, call *probeCall) bool {
	list := b.calls[workerID]
	i := slices.Index(list, call)
	if i < 0 {
		return false
	}
	if list = slices.Delete(list, i, i+1); len(list) == 0 {
		delete(b.calls, workerID)
	} else {
		b.calls[workerID] = list
	}
	return true
}

// probePassive calls the worker's Probe RPC through its loop's client.
func (b *ProbeBroker) probePassive(ctx context.Context, w workers.Worker, run *syncrv1.RunProbe, wait time.Duration) (*syncrv1.ProbeResult, error) {
	b.mu.Lock()
	reg := b.passive[w.ID]
	switch {
	case reg == nil:
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: the controller's link to worker %q is not running", domain.ErrUnavailable, w.Name)
	case b.inFlight[w.ID] >= b.cfg.MaxInFlightPerWorker:
		b.mu.Unlock()
		return nil, b.busy(w)
	}
	b.inFlight[w.ID]++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.inFlight[w.ID]--; b.inFlight[w.ID] <= 0 {
			delete(b.inFlight, w.ID)
		}
		b.mu.Unlock()
	}()

	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := reg.client.Probe(cctx, &syncrv1.ProbeRequest{Probe: run})
	switch {
	case err == nil && resp.GetResult() != nil:
		res := resp.GetResult()
		if res.GetProbeId() == "" {
			res.ProbeId = run.GetProbeId()
		}
		return res, nil
	case err == nil:
		return nil, fmt.Errorf("%w: worker %q answered without a result", domain.ErrUnavailable, w.Name)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("probe worker %s: %w", w.ID, ctx.Err())
	case errors.Is(err, ErrProbeUnsupported):
		return nil, fmt.Errorf("%w: worker %q (version %s) cannot run %s; upgrade it",
			domain.ErrFailedPrecondition, w.Name, cmp.Or(w.Info.Version, "unknown"), link.ProbeOpOf(run.GetSpec()))
	case cctx.Err() != nil:
		return nil, fmt.Errorf("%w: worker %q did not answer in time", domain.ErrUnavailable, w.Name)
	default:
		// The transport's error may name addresses: log it, do not return it.
		b.log.WarnContext(ctx, "probe call to a passive worker failed", "worker_id", w.ID.String(), "error", err.Error())
		return nil, fmt.Errorf("%w: the controller cannot reach worker %q", domain.ErrUnavailable, w.Name)
	}
}

func (b *ProbeBroker) busy(w workers.Worker) error {
	return fmt.Errorf("%w: worker %q is running too many requests at once; try again in a moment", domain.ErrUnavailable, w.Name)
}

// register makes client the worker's passive probe client until the
// returned function is called (which removes only this registration).
func (b *ProbeBroker) register(workerID domain.WorkerID, client PassiveClient) (unregister func()) {
	reg := &passiveRegistration{client: client}
	b.mu.Lock()
	b.passive[workerID] = reg
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		if b.passive[workerID] == reg {
			delete(b.passive, workerID)
		}
		b.mu.Unlock()
	}
}

// commands returns at most limit probe commands of the worker: a
// cancel_probe for every abandoned probe not acked yet, then a run_probe
// for at most maxProbeCommands probes it has not accepted, oldest first.
func (b *ProbeBroker) commands(workerID domain.WorkerID, limit int) []*syncrv1.Command {
	b.mu.Lock()
	defer b.mu.Unlock()
	var cmds []*syncrv1.Command
	for _, id := range b.cancels[workerID] {
		if len(cmds) >= limit {
			return cmds
		}
		cmds = append(cmds, &syncrv1.Command{
			Id:      probeCancelPrefix + id,
			Command: &syncrv1.Command_CancelProbe{CancelProbe: &syncrv1.CancelProbe{ProbeId: id}},
		})
	}
	n := 0
	for _, c := range b.calls[workerID] {
		if len(cmds) >= limit || n >= maxProbeCommands {
			break
		}
		if c.delivered {
			continue
		}
		n++
		cmds = append(cmds, &syncrv1.Command{Id: probePrefix + c.id, Command: &syncrv1.Command_RunProbe{RunProbe: c.run}})
	}
	return cmds
}

// ack applies the ack of a probe command; it reports whether cmdID was
// one.
func (b *ProbeBroker) ack(workerID domain.WorkerID, cmdID string) bool {
	switch {
	case strings.HasPrefix(cmdID, probeCancelPrefix):
		id := strings.TrimPrefix(cmdID, probeCancelPrefix)
		b.mu.Lock()
		if list := slices.DeleteFunc(b.cancels[workerID], func(c string) bool { return c == id }); len(list) == 0 {
			delete(b.cancels, workerID)
		} else {
			b.cancels[workerID] = list
		}
		b.mu.Unlock()
		return true
	case strings.HasPrefix(cmdID, probePrefix):
		id := strings.TrimPrefix(cmdID, probePrefix)
		b.mu.Lock()
		for _, c := range b.calls[workerID] {
			if c.id == id {
				c.delivered = true
			}
		}
		b.mu.Unlock()
		return true
	}
	return false
}

// epochChanged handles a new worker process: the probes its predecessor
// accepted died with it and fail; the others are sent to the new process.
// Its pending cancels are moot.
func (b *ProbeBroker) epochChanged(workerID domain.WorkerID, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cancels, workerID)
	kept := b.calls[workerID][:0:0]
	for _, c := range b.calls[workerID] {
		if c.delivered {
			c.done <- probeOutcome{err: fmt.Errorf("%w: worker %q restarted before it answered", domain.ErrUnavailable, name)}
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		delete(b.calls, workerID)
	} else {
		b.calls[workerID] = kept
	}
}

// complete hands a worker's result to the waiting call; false if the
// worker has no such probe (unknown, finished, or another worker's).
func (b *ProbeBroker) complete(workerID domain.WorkerID, r *syncrv1.ProbeResult) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls[workerID] {
		if c.id == r.GetProbeId() {
			b.removeLocked(workerID, c)
			c.done <- probeOutcome{result: r}
			return true
		}
	}
	return false
}

// CompleteProbe delivers the result of a probe that worker workerID (in
// epoch) received as Command.run_probe (ControllerLinkService.
// CompleteProbe). It authenticates and adopts the epoch like Report
// (ErrStaleEpoch for a replaced one), then hands r to the broker: a result
// of an unknown, finished or another worker's probe is ignored (nil), so
// retries are idempotent. domain.ErrInvalidArgument for a result without a
// probe ID.
func (s *Service) CompleteProbe(ctx context.Context, workerID domain.WorkerID, epoch string, r *syncrv1.ProbeResult) error {
	if r.GetProbeId() == "" {
		return fmt.Errorf("complete a probe of worker %s: %w: no probe ID", workerID, domain.ErrInvalidArgument)
	}
	sess, _, err := s.begin(ctx, workerID, epoch)
	if err != nil {
		return err
	}
	sess.mu.Unlock()
	if s.probes == nil || !s.probes.complete(workerID, r) {
		s.log.DebugContext(ctx, "ignoring the result of an unknown probe", "worker_id", workerID.String(), "probe_id", r.GetProbeId())
	}
	return nil
}
