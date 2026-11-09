// Package agent executes the controller's commands on a worker: it accepts
// StartJob and CancelJob commands, runs jobs through the runner registry
// under a concurrency limit, and reports what happens as events in the
// worker's outbox (internal/worker/outbox).
//
// # Commands
//
// HandleCommands acknowledges every command that has an ID, except a
// StartJob while the agent shuts down (see Shutdown). An ack means
// "accepted", never "succeeded": outcomes arrive as events. Handling is
// idempotent, because the controller redelivers commands until it receives
// their ack:
//
//   - StartJob for a job ID the agent already knows in this epoch (queued,
//     running or finished) is acked and ignored. The result of a finished job
//     stays in the outbox until the controller acks it.
//   - StartJob whose spec has no runner (no operation, unsupported kind) is
//     acked and reported at once as a FAILED JobResult.
//   - CancelJob for a queued job removes it from the queue and reports a
//     CANCELLED JobResult; for a running job it cancels the runner's
//     context (the runner stops its tool with the supervisor's grace period)
//     and the job reports CANCELLED when the runner returns; for an unknown or
//     finished job it is acked and ignored.
//
// # Events of one job
//
// A job that runs produces JobStarted, then Progress events (at most one per
// ProgressInterval, and the last sample the runner emitted is always
// delivered before the result), then exactly one JobResult. A job that never
// started (invalid spec, cancelled while queued, worker shutting down)
// produces only its JobResult. JobStarted means "handed to the runner": a
// runner may still refuse the spec (e.g. a path outside local_roots) before
// its tool process starts.
//
// # Concurrency and backpressure
//
// At most MaxConcurrentJobs jobs run at once; further jobs wait in a local
// FIFO queue and start in that order (their JobStarted events too). A
// queued job also waits while the outbox is at its high-water mark
// (outbox.Stats().Saturated), so a controller that stops acking events
// stops the admission of new work instead of losing results; results of
// running jobs are always appended.
//
// # Shutdown
//
// Shutdown cancels queued and running jobs, waits for their CANCELLED
// results to be in the outbox, and from then on refuses StartJob: it is
// not acked and leaves no event, so the controller keeps the job queued
// for the worker's next process. (The CANCELLED result of a job that was
// still queued has no start time and no JobStarted before it, which tells
// the controller that it never ran, so it queues it again.) The transport
// then tries a final flush (see internal/worker/linkcore).
//
// # Results
//
// A CANCELLED result carries code CANCELLED (CancelJob) or WORKER_SHUTDOWN
// (Shutdown) with the cause as message. Result sizes are bounded so that no
// event approaches the link's read limits: error messages to
// MaxErrorMessageBytes, warnings by runner.LimitWarnings and the
// kind-specific output by Config.MaxOutputBytes (truncated with a warning).
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/link, internal/worker/runner (the contract and registry, not the
// runner implementations) and internal/worker/outbox.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/outbox"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Defaults for Config.
const (
	// DefaultMaxConcurrentJobs is the default number of jobs run at once.
	DefaultMaxConcurrentJobs = 2
	// DefaultProgressInterval is the default minimum time between two
	// Progress events of one job.
	DefaultProgressInterval = time.Second
	// DefaultMaxOutputBytes is the default bound on the encoded JobOutput of
	// one result (see Config.MaxOutputBytes). With it, no single event comes
	// near the link's 16 MiB read limits.
	DefaultMaxOutputBytes = 4 << 20
	// MaxErrorMessageBytes bounds the error message of a result.
	MaxErrorMessageBytes = 4096
)

// Cancellation causes, reported as the message of CANCELLED results.
var (
	// ErrCancelled is the cause of a job stopped by a CancelJob command.
	ErrCancelled = errors.New("job cancelled by the controller")
	// ErrShuttingDown is the cause of a job stopped because the worker is
	// shutting down.
	ErrShuttingDown = errors.New("job cancelled because the worker is shutting down")
)

// Runners resolves the runner of a job spec. *runner.Registry implements it.
type Runners interface {
	// ForSpec returns the runner for spec's kind, or an error (with a
	// runner.CodeOf code) to report as the job's failure.
	ForSpec(spec *syncrv1.JobSpec) (runner.Runner, error)
	// Kinds returns the job kinds the worker can run, for the Hello event.
	Kinds() []string
}

// Tool is a supervised external tool and its resolved version.
type Tool struct {
	Name    string
	Version string
}

// Identity describes the worker process in its Hello event. The composition
// root fills it from the build, the host and tool resolution.
type Identity struct {
	// WorkerVersion is the worker build version (e.g. "v2.0.0" or "dev").
	WorkerVersion string
	// OS and Arch are runtime.GOOS and runtime.GOARCH.
	OS, Arch string
	// Hostname is the machine's host name, for display only.
	Hostname string
	// Tools are the resolved tool versions.
	Tools []Tool
	// LocalRoots are the configured local roots.
	LocalRoots []string
}

// Config configures an Agent.
type Config struct {
	// Runners resolves job specs to runners. Required.
	Runners Runners

	// Identity is reported in the Hello event.
	Identity Identity

	// MaxConcurrentJobs caps the jobs that run at once. Zero means
	// DefaultMaxConcurrentJobs.
	MaxConcurrentJobs int

	// ProgressInterval is the minimum time between two Progress events of
	// one job. Zero means DefaultProgressInterval.
	ProgressInterval time.Duration

	// Outbox configures the event outbox the agent creates.
	Outbox outbox.Options

	// MaxOutputBytes bounds the encoded kind-specific output of a result. A
	// larger output is truncated (e.g. only the newest restic snapshots are
	// listed) with a warning saying so. Zero means DefaultMaxOutputBytes.
	MaxOutputBytes int

	// Logger receives operational logs (never job specs or secrets). Nil
	// discards them.
	Logger *slog.Logger
}

// jobState is where a job is in its life on the worker.
type jobState int

const (
	// stateQueued: waiting in the local queue for a run slot and outbox
	// capacity.
	stateQueued jobState = iota
	// stateRunning: holds a run slot and was handed to its runner
	// (JobStarted was emitted).
	stateRunning
)

// job is a job the agent holds (queued, admitted or running).
type job struct {
	id     string
	spec   *syncrv1.JobSpec
	runner runner.Runner
	state  jobState
	// ctx is cancelled (with ErrCancelled or ErrShuttingDown) to stop the
	// job; cancel also releases its resources once the job has finished.
	ctx       context.Context
	cancel    context.CancelCauseFunc
	startedAt time.Time
}

// Agent runs jobs on behalf of the controller. Create it with New.
type Agent struct {
	runners          Runners
	maxConcurrent    int
	progressInterval time.Duration
	maxOutputBytes   int
	out              *outbox.Outbox
	log              *slog.Logger

	// base is the parent of every job context; stop cancels it on shutdown.
	base context.Context
	stop context.CancelCauseFunc
	// wg tracks job goroutines.
	wg sync.WaitGroup

	mu sync.Mutex
	// active holds queued and running jobs; order lists their IDs in
	// acceptance order.
	active map[string]*job
	order  []string
	// queue holds jobs waiting for a run slot, oldest first.
	queue []*job
	// slots counts running jobs.
	slots int
	// waiting reports that a goroutine waits for outbox capacity to resume
	// admission.
	waiting bool
	// finished holds the IDs of every job that finished in this epoch, so
	// duplicate StartJobs are ignored (about 50 bytes per job).
	finished map[string]struct{}
	stopping bool
}

// New returns an Agent and creates its outbox, whose first event is the
// Hello built from cfg.
func New(cfg Config) (*Agent, error) {
	if cfg.Runners == nil {
		return nil, errors.New("agent: runners are nil")
	}
	if cfg.MaxConcurrentJobs < 0 {
		return nil, fmt.Errorf("agent: max concurrent jobs is %d", cfg.MaxConcurrentJobs)
	}
	if cfg.ProgressInterval < 0 {
		return nil, fmt.Errorf("agent: progress interval is %s", cfg.ProgressInterval)
	}
	if cfg.MaxOutputBytes < 0 {
		return nil, fmt.Errorf("agent: max output bytes is %d", cfg.MaxOutputBytes)
	}
	a := &Agent{
		runners:          cfg.Runners,
		maxConcurrent:    cfg.MaxConcurrentJobs,
		progressInterval: cfg.ProgressInterval,
		maxOutputBytes:   cmp.Or(cfg.MaxOutputBytes, DefaultMaxOutputBytes),
		log:              cfg.Logger,
		active:           make(map[string]*job),
		finished:         make(map[string]struct{}),
	}
	if a.maxConcurrent == 0 {
		a.maxConcurrent = DefaultMaxConcurrentJobs
	}
	if a.progressInterval == 0 {
		a.progressInterval = DefaultProgressInterval
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	a.base, a.stop = context.WithCancelCause(context.Background())
	a.out = outbox.New(a.hello(cfg.Identity), cfg.Outbox)
	return a, nil
}

// hello builds the Hello event payload.
func (a *Agent) hello(id Identity) *syncrv1.Hello {
	h := &syncrv1.Hello{
		WorkerVersion:     id.WorkerVersion,
		Os:                id.OS,
		Arch:              id.Arch,
		Hostname:          id.Hostname,
		JobKinds:          a.runners.Kinds(),
		MaxConcurrentJobs: int32(min(a.maxConcurrent, 1<<31-1)), //nolint:gosec // clamped to the int32 range
		LocalRoots:        slices.Clone(id.LocalRoots),
	}
	for _, t := range id.Tools {
		h.Tools = append(h.Tools, &syncrv1.ToolVersion{Name: t.Name, Version: t.Version})
	}
	return h
}

// Outbox returns the agent's event outbox.
func (a *Agent) Outbox() *outbox.Outbox { return a.out }

// Epoch returns the epoch of the agent's outbox.
func (a *Agent) Epoch() string { return a.out.Epoch() }

// HandleCommands processes cmds in order and returns one ack per accepted
// command that has an ID, in the same order. It never blocks on job
// execution. Commands without an ID cannot be acknowledged and are ignored;
// commands of a type this worker does not know are acked (so the controller
// stops redelivering them) and ignored. Once Shutdown began, StartJob
// commands are refused: they get no ack, so the controller keeps the job
// queued and redelivers it (to the worker's next process, eventually).
func (a *Agent) HandleCommands(ctx context.Context, cmds []*syncrv1.Command) []*syncrv1.CommandAck {
	acks := make([]*syncrv1.CommandAck, 0, len(cmds))
	for _, cmd := range cmds {
		if cmd.GetId() == "" {
			a.log.WarnContext(ctx, "ignoring command without id")
			continue
		}
		switch c := cmd.GetCommand().(type) {
		case *syncrv1.Command_StartJob:
			if !a.startJob(ctx, c.StartJob) {
				continue // refused: no ack
			}
		case *syncrv1.Command_CancelJob:
			a.cancelJob(ctx, c.CancelJob.GetJobId())
		default:
			a.log.WarnContext(ctx, "ignoring unknown command", "command_id", cmd.GetId())
		}
		acks = append(acks, &syncrv1.CommandAck{CommandId: cmd.GetId()})
	}
	return acks
}

// startJob handles a StartJob and reports whether it was accepted (and is
// to be acked). It is refused only while the agent shuts down, also for a
// job it knows: the controller keeps a job it has not seen accepted queued,
// and settles one it has seen accepted by the job's result.
func (a *Agent) startJob(ctx context.Context, sj *syncrv1.StartJob) bool {
	id := sj.GetJobId()
	if id == "" {
		a.log.WarnContext(ctx, "ignoring start job command without job id")
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		// Debug only: a passive controller redelivers it every round of the
		// final flush.
		a.log.DebugContext(ctx, "refusing start job command: the worker is shutting down", "job_id", id)
		return false
	}
	if a.knownLocked(id) {
		a.log.DebugContext(ctx, "duplicate start job command", "job_id", id)
		return true
	}
	rn, err := a.runners.ForSpec(sj.GetSpec())
	if err != nil {
		a.log.WarnContext(ctx, "job rejected", "job_id", id, "kind", link.KindOf(sj.GetSpec()), "error", err.Error())
		a.finishLocked(id, failedResult(id, runner.CodeOf(err), err.Error(), time.Time{}, runner.Result{ExitCode: -1}))
		return true
	}
	j := &job{id: id, spec: sj.GetSpec(), runner: rn, state: stateQueued}
	j.ctx, j.cancel = context.WithCancelCause(a.base)
	a.active[id] = j
	a.order = append(a.order, id)
	a.queue = append(a.queue, j)
	a.log.InfoContext(ctx, "job accepted", "job_id", id, "kind", rn.Kind())
	a.admitLocked()
	return true
}

func (a *Agent) cancelJob(ctx context.Context, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.active[id]
	if !ok {
		a.log.DebugContext(ctx, "cancel for unknown or finished job", "job_id", id)
		return
	}
	a.log.InfoContext(ctx, "stopping job on request", "job_id", id)
	a.cancelLocked(j, ErrCancelled)
}

// cancelLocked stops j: a queued job finishes at once, a running job
// through its goroutine.
func (a *Agent) cancelLocked(j *job, cause error) {
	if j.state == stateQueued {
		a.queue = slices.DeleteFunc(a.queue, func(q *job) bool { return q == j })
		j.cancel(cause)
		a.finishJobLocked(j, cancelledResult(j.id, cause, time.Time{}, runner.Result{ExitCode: -1}))
		return
	}
	j.cancel(cause)
}

// knownLocked reports whether the agent holds or has finished job id.
func (a *Agent) knownLocked(id string) bool {
	if _, ok := a.active[id]; ok {
		return true
	}
	_, ok := a.finished[id]
	return ok
}

// admitLocked starts queued jobs, oldest first, while run slots are free and
// the outbox is below its high-water mark. Starting happens under the lock,
// so JobStarted events follow the queue order. When the outbox is
// saturated, one goroutine waits for capacity and resumes admission.
func (a *Agent) admitLocked() {
	for !a.stopping && a.slots < a.maxConcurrent && len(a.queue) > 0 {
		if a.out.Stats().Saturated {
			a.awaitCapacityLocked()
			return
		}
		j := a.queue[0]
		a.queue[0] = nil
		a.queue = a.queue[1:]
		a.slots++
		j.state = stateRunning
		j.startedAt = time.Now()
		a.appendLocked(&syncrv1.Event{Payload: &syncrv1.Event_JobStarted{JobStarted: &syncrv1.JobStarted{JobId: j.id}}})
		a.wg.Go(func() { a.run(j) })
	}
}

// awaitCapacityLocked starts the goroutine that resumes admission once the
// outbox has capacity again, unless it already runs. It ends early on
// shutdown.
func (a *Agent) awaitCapacityLocked() {
	if a.waiting {
		return
	}
	a.waiting = true
	a.wg.Go(func() {
		err := a.out.WaitCapacity(a.base)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.waiting = false
		if err == nil {
			a.admitLocked()
		}
	})
}

// run executes a started job on its own goroutine.
func (a *Agent) run(j *job) {
	th := newThrottle(a.progressInterval, func(p runner.Progress) {
		a.append(&syncrv1.Event{Payload: &syncrv1.Event_Progress{Progress: &syncrv1.Progress{JobId: j.id, Stats: p.Proto()}}})
	})
	res, err := a.invoke(j, th.emit)
	last, seen := th.close()

	var result *syncrv1.JobResult
	switch {
	case errors.Is(err, errRunnerPanicked):
		res = runner.Result{ExitCode: -1}
		if seen {
			res.Stats = last
		}
		result = failedResult(j.id, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL,
			"internal worker error: the job runner crashed", j.startedAt, res)
	case err == nil:
		result = succeededResult(j.id, j.startedAt, res, a.maxOutputBytes)
	case j.ctx.Err() != nil:
		result = cancelledResult(j.id, context.Cause(j.ctx), j.startedAt, res)
	default:
		result = failedResult(j.id, runner.CodeOf(err), err.Error(), j.startedAt, res)
	}

	a.mu.Lock()
	a.finishJobLocked(j, result)
	a.mu.Unlock()
}

// errRunnerPanicked is returned by invoke when the runner panicked.
var errRunnerPanicked = errors.New("runner panicked")

// invoke runs j's runner, turning a panic into errRunnerPanicked.
func (a *Agent) invoke(j *job, emit runner.EmitFunc) (res runner.Result, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = errRunnerPanicked
			a.log.Error("job runner panicked", "job_id", j.id, "kind", j.runner.Kind(),
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		}
	}()
	return j.runner.Run(j.ctx, runner.Job{ID: j.id, Spec: j.spec}, emit)
}

// finishJobLocked removes j from the held jobs and reports result.
func (a *Agent) finishJobLocked(j *job, result *syncrv1.JobResult) {
	if j.state == stateRunning {
		a.slots--
	}
	delete(a.active, j.id)
	a.order = slices.DeleteFunc(a.order, func(id string) bool { return id == j.id })
	j.cancel(nil) // release the context; the cause of an earlier cancel stays
	j.spec, j.runner = nil, nil
	a.finishLocked(j.id, result)
	a.log.Info("job finished", "job_id", j.id, "status", result.GetStatus().String(),
		"error_code", result.GetError().GetCode().String())
	a.admitLocked()
}

// finishLocked records id as finished and appends its result.
func (a *Agent) finishLocked(id string, result *syncrv1.JobResult) {
	a.finished[id] = struct{}{}
	a.appendLocked(&syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: result}})
}

// appendLocked appends ev; the agent's lock orders it with state changes.
func (a *Agent) appendLocked(ev *syncrv1.Event) { a.append(ev) }

func (a *Agent) append(ev *syncrv1.Event) {
	if _, err := a.out.Append(ev); err != nil {
		// Only events without a payload or Hello events are refused, which
		// the agent never builds.
		panic(fmt.Sprintf("agent: outbox refused an event: %v", err))
	}
}

// State returns the jobs the agent holds (queued, waiting to start or
// running), in the order they were accepted.
func (a *Agent) State() *syncrv1.WorkerState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *Agent) stateLocked() *syncrv1.WorkerState {
	st := &syncrv1.WorkerState{Jobs: make([]*syncrv1.ActiveJob, 0, len(a.order))}
	for _, id := range a.order {
		j := a.active[id]
		aj := &syncrv1.ActiveJob{JobId: id}
		if j.state == stateRunning {
			aj.StartedAt = stamp(j.startedAt)
		}
		st.Jobs = append(st.Jobs, aj)
	}
	return st
}

// Snapshot is a consistent view of the outbox and the agent's jobs, taken
// for one Report or Pull.
type Snapshot struct {
	// Epoch is the outbox epoch.
	Epoch string
	// Events are unacked events after the requested seq, in seq order. They
	// are shared with the outbox and must not be modified.
	Events []*syncrv1.Event
	// More reports that further events are pending after Events.
	More bool
	// State lists the jobs the worker holds as of the last event in Events:
	// the jobs it holds now plus the jobs whose JobResult is pending in the
	// outbox but not part of Events. So a job missing from State always has
	// its result in Events or already acked, and a job whose result is in
	// Events is never listed.
	State *syncrv1.WorkerState
}

// Snapshot returns up to limit events with seq > afterSeq (limit <= 0: all)
// whose encoded sizes sum to at most maxBytes (<= 0: unbounded; the first
// pending event is always included, see outbox.ReadBounded), and the
// matching worker state, read atomically. The state's last_seq is the seq of
// the last returned event or, if none is returned, the last seq assigned in
// the epoch; outbox_backlog and outbox_saturated describe the whole outbox.
func (a *Agent) Snapshot(afterSeq int64, limit, maxBytes int) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.out.ReadBounded(afterSeq, limit, maxBytes)
	st := a.stateLocked()
	for _, r := range b.Withheld {
		st.Jobs = append(st.Jobs, &syncrv1.ActiveJob{JobId: r.GetJobId(), StartedAt: r.GetStartedAt()})
	}
	st.LastSeq = b.Head
	if n := len(b.Events); n > 0 {
		st.LastSeq = b.Events[n-1].GetSeq()
	}
	st.OutboxBacklog = int32(min(b.Backlog, 1<<31-1)) //nolint:gosec // clamped to the int32 range
	st.OutboxSaturated = b.Saturated
	return Snapshot{Epoch: a.out.Epoch(), Events: b.Events, More: b.More, State: st}
}

// AckEvents acknowledges events up to and including seq (outbox.Ack).
func (a *Agent) AckEvents(seq int64) error {
	_, err := a.out.Ack(seq)
	return err
}

// Shutdown stops the agent: queued jobs are cancelled at once, running jobs
// are cancelled through their runners (which stop their tools gracefully,
// then kill them after the supervisor's grace period), and later StartJob
// commands are refused (not acked, no event). It waits until every job has
// reported its result to the outbox, or until ctx is done, in which case it
// returns an error wrapping context.Cause(ctx). Pass a ctx that outlasts the
// supervisor's grace period. Calling Shutdown again waits again.
func (a *Agent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if !a.stopping {
		a.stopping = true
		a.log.InfoContext(ctx, "agent shutting down", "jobs", len(a.active))
		for _, j := range slices.Clone(a.queue) {
			a.cancelLocked(j, ErrShuttingDown)
		}
		a.stop(ErrShuttingDown)
	}
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("agent: shutdown: jobs still running: %w", context.Cause(ctx))
	}
}

// Result builders. Warnings and output are reported only for SUCCEEDED
// results, matching the JobResult contract.

func succeededResult(id string, startedAt time.Time, res runner.Result, maxOutputBytes int) *syncrv1.JobResult {
	r := baseResult(id, syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, startedAt, res)
	warnings := res.Warnings
	var truncated string
	r.Output, truncated = limitOutput(res.Output, maxOutputBytes)
	if truncated != "" {
		// First, so the bound on the number of warnings never drops it.
		warnings = append([]string{truncated}, warnings...)
	}
	r.Warnings = runner.LimitWarnings(warnings)
	return r
}

func failedResult(id string, code syncrv1.JobErrorCode, msg string, startedAt time.Time, res runner.Result) *syncrv1.JobResult {
	r := baseResult(id, syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED, startedAt, res)
	r.Error = &syncrv1.JobError{Code: code, Message: clip(msg, MaxErrorMessageBytes)}
	return r
}

// cancelledResult reports a cancelled job. The error code is CANCELLED for a
// CancelJob command and WORKER_SHUTDOWN for a shutdown; the message carries
// the cause.
func cancelledResult(id string, cause error, startedAt time.Time, res runner.Result) *syncrv1.JobResult {
	if cause == nil {
		cause = ErrCancelled
	}
	code := syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED
	if errors.Is(cause, ErrShuttingDown) {
		code = syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN
	}
	r := baseResult(id, syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED, startedAt, res)
	r.Error = &syncrv1.JobError{Code: code, Message: clip(cause.Error(), MaxErrorMessageBytes)}
	return r
}

func baseResult(id string, status syncrv1.JobResultStatus, startedAt time.Time, res runner.Result) *syncrv1.JobResult {
	return &syncrv1.JobResult{
		JobId:      id,
		Status:     status,
		Stats:      res.Stats.Proto(),
		ExitCode:   int32(max(min(res.ExitCode, 1<<31-1), -1<<31)),
		StartedAt:  stamp(startedAt),
		FinishedAt: timestamppb.Now(),
	}
}

// stamp converts t, returning nil for the zero time.
func stamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
