package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Command ID prefixes. A command's ID is derived from its job ("start:<job
// id>", "cancel:<job id>"), so a redelivered command keeps its ID and an ack
// identifies the job without any stored command record.
const (
	startPrefix  = "start:"
	cancelPrefix = "cancel:"
)

// maxAttempts bounds compare-and-swap retries on the worker's epoch.
const maxAttempts = 8

// Report handles ControllerLinkService.Report for an authenticated active
// worker: adopt the epoch, apply acks, ingest events, reconcile the state.
func (s *Service) Report(ctx context.Context, workerID domain.WorkerID, req *syncrv1.ReportRequest) (*syncrv1.ReportResponse, error) {
	sess, w, err := s.begin(ctx, workerID, req.GetEpoch())
	if err != nil {
		return nil, err
	}
	defer sess.mu.Unlock()
	if err := s.ack(ctx, sess, workerID, req.GetEpoch(), req.GetAcks()); err != nil {
		return nil, err
	}
	ackSeq, err := s.ingest(ctx, sess, w, req.GetEpoch(), req.GetEvents(), req.GetState())
	if err != nil {
		return nil, err
	}
	return &syncrv1.ReportResponse{AckSeq: ackSeq}, nil
}

// Poll handles ControllerLinkService.Poll for an authenticated active
// worker: apply the acks, then wait for commands (WaitCommands). An unset
// wait means Config.MaxWait.
func (s *Service) Poll(ctx context.Context, workerID domain.WorkerID, req *syncrv1.PollRequest) (*syncrv1.PollResponse, error) {
	if err := s.Ack(ctx, workerID, req.GetEpoch(), req.GetAcks()); err != nil {
		return nil, err
	}
	wait := s.cfg.MaxWait
	if req.GetWait() != nil {
		wait = req.GetWait().AsDuration()
	}
	cmds, err := s.WaitCommands(ctx, workerID, req.GetEpoch(), wait)
	if err != nil {
		return nil, err
	}
	return &syncrv1.PollResponse{Commands: cmds}, nil
}

// Ingest applies a batch of the worker's outbox events from epoch and
// reconciles its state snapshot (nil if none). It is idempotent by (epoch,
// seq): events at or below the stored cursor are skipped, the rest are
// applied in seq order (gaps are fine). It returns the cursor after the
// batch, which is what the worker may drop events up to. If applying an
// event fails, the cursor stops before it and the error is returned; the
// worker resends from there.
func (s *Service) Ingest(ctx context.Context, workerID domain.WorkerID, epoch string, events []*syncrv1.Event, state *syncrv1.WorkerState) (int64, error) {
	sess, w, err := s.begin(ctx, workerID, epoch)
	if err != nil {
		return 0, err
	}
	defer sess.mu.Unlock()
	return s.ingest(ctx, sess, w, epoch, events, state)
}

// Ack applies command acknowledgements from epoch. Acks are idempotent and
// may repeat across Report and Poll: an acked StartJob marks its job
// dispatched (if still queued), an acked CancelJob is not sent again in
// this epoch. Unknown command IDs are ignored.
func (s *Service) Ack(ctx context.Context, workerID domain.WorkerID, epoch string, acks []*syncrv1.CommandAck) error {
	sess, _, err := s.begin(ctx, workerID, epoch)
	if err != nil {
		return err
	}
	defer sess.mu.Unlock()
	return s.ack(ctx, sess, workerID, epoch, acks)
}

// Commands returns the worker's pending commands for epoch, oldest job
// first (see the package documentation for how they are derived). It never
// blocks.
func (s *Service) Commands(ctx context.Context, workerID domain.WorkerID, epoch string) ([]*syncrv1.Command, error) {
	sess, w, err := s.begin(ctx, workerID, epoch)
	if err != nil {
		return nil, err
	}
	defer sess.mu.Unlock()
	return s.commands(ctx, sess, w, epoch)
}

// WaitCommands returns the worker's pending commands as soon as there are
// any, or nil when wait (capped at Config.MaxWait; <= 0 means do not wait)
// elapses. It wakes up when the worker's commands may have changed (Hub)
// and never polls. It returns ctx's error if ctx ends first; transports tie
// ctx to the request and to server shutdown, and may answer a shutdown with
// an empty response.
func (s *Service) WaitCommands(ctx context.Context, workerID domain.WorkerID, epoch string, wait time.Duration) ([]*syncrv1.Command, error) {
	wait = min(wait, s.cfg.MaxWait)
	var expired <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		expired = t.C
	}
	for {
		woken := s.hub.wait(workerID)
		cmds, err := s.Commands(ctx, workerID, epoch)
		if err != nil || len(cmds) > 0 || wait <= 0 {
			return cmds, err
		}
		select {
		case <-woken:
		case <-expired:
			return nil, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for commands: %w", ctx.Err())
		}
	}
}

// begin locks the worker's session, adopts epoch (reconciling if it is
// new) and records the contact. On success the caller must unlock
// sess.mu.
func (s *Service) begin(ctx context.Context, workerID domain.WorkerID, epoch string) (*session, workers.Worker, error) {
	if err := workers.ValidEpoch(epoch); err != nil {
		return nil, workers.Worker{}, fmt.Errorf("worker %s: %w", workerID, err)
	}
	sess := s.session(workerID)
	sess.mu.Lock()
	w, err := s.observe(ctx, sess, workerID, epoch)
	if err == nil {
		err = s.workers.Touch(ctx, workerID)
	}
	if err != nil {
		sess.mu.Unlock()
		return nil, workers.Worker{}, err
	}
	return sess, w, nil
}

// observe returns the worker after making epoch its current epoch: a known
// current epoch is accepted, a replaced one is rejected (ErrStaleEpoch) and
// a new one is adopted by compare-and-swap, followed by the epoch
// reconcile.
func (s *Service) observe(ctx context.Context, sess *session, workerID domain.WorkerID, epoch string) (workers.Worker, error) {
	for range maxAttempts {
		w, err := s.workers.Get(ctx, workerID)
		if err != nil {
			return workers.Worker{}, err
		}
		switch {
		case w.Link.Epoch == epoch:
			if sess.epoch != epoch {
				sess.reset(epoch) // first call of this epoch in this process
			}
			return w, nil
		case slices.Contains(w.Link.PastEpochs, epoch):
			s.log.WarnContext(ctx, "rejecting request of a replaced worker process", "worker_id", workerID.String(), "epoch", epoch)
			return workers.Worker{}, fmt.Errorf("worker %s epoch %q: %w", workerID, epoch, errors.Join(ErrStaleEpoch, domain.ErrFailedPrecondition))
		}
		next, err := s.workers.AdvanceEpoch(ctx, w, epoch)
		if errors.Is(err, domain.ErrConflict) {
			continue // another request changed the epoch first; decide again
		}
		if err != nil {
			return workers.Worker{}, err
		}
		sess.reset(epoch)
		if s.probes != nil {
			s.probes.epochChanged(workerID, w.Name)
		}
		s.log.InfoContext(ctx, "worker process changed", "worker_id", workerID.String(), "epoch", epoch, "previous_epoch", w.Link.Epoch)
		js, err := s.jobs.Unfinished(ctx, workerID)
		if err != nil {
			return workers.Worker{}, err
		}
		if _, err := s.reconcileEpoch(ctx, workerID, epoch, js); err != nil {
			return workers.Worker{}, err
		}
		return next, nil
	}
	return workers.Worker{}, fmt.Errorf("worker %s: epoch changed concurrently %d times: %w", workerID, maxAttempts, domain.ErrConflict)
}

// ingest implements Ingest with the session locked.
func (s *Service) ingest(ctx context.Context, sess *session, w workers.Worker, epoch string, events []*syncrv1.Event, state *syncrv1.WorkerState) (int64, error) {
	sorted := slices.Clone(events)
	for _, ev := range sorted {
		if ev.GetSeq() <= 0 {
			return 0, fmt.Errorf("worker %s: %w: event seq %d must be positive", w.ID, domain.ErrInvalidArgument, ev.GetSeq())
		}
	}
	slices.SortStableFunc(sorted, func(a, b *syncrv1.Event) int { return cmp.Compare(a.GetSeq(), b.GetSeq()) })

	cursor, applied := w.Link.AckSeq, w.Link.AckSeq
	var applyErr error
	for _, ev := range sorted {
		if ev.GetSeq() <= applied {
			continue // already stored (duplicate or redelivery)
		}
		if applyErr = s.apply(ctx, w.ID, epoch, ev); applyErr != nil {
			break
		}
		if h := ev.GetHello(); h != nil {
			if s.probes != nil {
				// The probes this process cannot run fail now instead of
				// waiting for their timeout.
				s.probes.refuse(w.ID, w.Name, infoFromHello(h, timeOf(ev.GetTime())))
			}
			if sess.awaitsHello {
				// A derivation held back StartJob and probe commands for
				// this Hello (see commands): wake the worker's long poll.
				sess.awaitsHello = false
				s.hub.Wake(w.ID)
			}
		}
		if !sess.stopping && ev.GetJobResult().GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
			sess.stopping = true
			s.log.InfoContext(ctx, "worker process is shutting down; it gets no more jobs", "worker_id", w.ID.String(), "epoch", epoch)
		}
		applied = ev.GetSeq()
	}
	if applied > cursor {
		var err error
		if cursor, err = s.workers.AdvanceAckSeq(ctx, w.ID, epoch, applied); err != nil {
			return 0, errors.Join(applyErr, err)
		}
	}
	if applyErr != nil {
		return cursor, applyErr
	}
	if state != nil {
		snapSeq := state.GetLastSeq()
		if snapSeq == 0 && len(sorted) > 0 {
			snapSeq = sorted[len(sorted)-1].GetSeq()
		}
		if err := s.reconcileSnapshot(ctx, sess, w.ID, epoch, cursor, snapSeq, state); err != nil {
			return cursor, err
		}
	}
	return cursor, nil
}

// apply applies one event. Events about jobs the worker may not touch
// (unknown, another worker's, malformed IDs) and events of unknown types are
// skipped: they are acknowledged so they cannot block the outbox, and the
// snapshot reconcile cancels any such job the worker still runs. Only store
// failures are returned.
func (s *Service) apply(ctx context.Context, workerID domain.WorkerID, epoch string, ev *syncrv1.Event) error {
	at := timeOf(ev.GetTime())
	var (
		jobID string
		err   error
	)
	switch p := ev.GetPayload().(type) {
	case *syncrv1.Event_Hello:
		return s.workers.SetInfo(ctx, workerID, epoch, infoFromHello(p.Hello, at))
	case *syncrv1.Event_JobStarted:
		jobID = p.JobStarted.GetJobId()
		var id domain.JobID
		if id, err = domain.ParseJobID(jobID); err == nil {
			_, _, err = s.jobs.MarkRunning(ctx, workerID, id, epoch, at)
		}
	case *syncrv1.Event_Progress:
		jobID = p.Progress.GetJobId()
		var id domain.JobID
		if id, err = domain.ParseJobID(jobID); err == nil {
			_, err = s.jobs.RecordProgress(ctx, workerID, id, epoch, jobs.Sample{Time: at, Stats: statsFromProto(p.Progress.GetStats()), Phase: progressPhase(p.Progress.GetPhase())})
		}
	case *syncrv1.Event_JobResult:
		jobID = p.JobResult.GetJobId()
		var id domain.JobID
		if id, err = domain.ParseJobID(jobID); err == nil {
			_, _, err = s.jobs.Finish(ctx, workerID, id, epoch, outcomeFromResult(p.JobResult))
		}
	default:
		s.log.WarnContext(ctx, "skipping event of unknown type", "worker_id", workerID.String(), "seq", ev.GetSeq())
		return nil
	}
	if errors.Is(err, domain.ErrInvalidID) || errors.Is(err, domain.ErrNotFound) {
		s.log.WarnContext(ctx, "skipping event for a job the worker does not own",
			"worker_id", workerID.String(), "seq", ev.GetSeq(), "job_id", jobID)
		return nil
	}
	return err
}

// ack implements Ack with the session locked.
func (s *Service) ack(ctx context.Context, sess *session, workerID domain.WorkerID, epoch string, acks []*syncrv1.CommandAck) error {
	for _, a := range acks {
		cmdID := a.GetCommandId()
		switch {
		case s.probes != nil && s.probes.ack(workerID, cmdID):
		case strings.HasPrefix(cmdID, startPrefix):
			id, err := domain.ParseJobID(strings.TrimPrefix(cmdID, startPrefix))
			if err != nil || sess.stopping {
				// A stopping process's start ack is late: the job's own
				// result settles it (see Shutdown in the package
				// documentation).
				continue
			}
			if _, _, err := s.jobs.MarkDispatched(ctx, workerID, id, epoch); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		case strings.HasPrefix(cmdID, cancelPrefix):
			sess.cancelAcked[strings.TrimPrefix(cmdID, cancelPrefix)] = true
		default:
			s.log.DebugContext(ctx, "ignoring ack of unknown command", "worker_id", workerID.String(), "command_id", cmdID)
		}
	}
	return nil
}

// commands implements Commands with the session locked; w is the worker as
// read for this call (in epoch). It returns at most Config.MaxCommands
// commands, taken in this order of priority:
//
//  1. CancelJob: the worker's jobs with a cancel request, oldest first, then
//     its orphans. Someone asked for the work to stop, and they are few.
//  2. Probe commands (active workers), up to half of the room the cancels
//     leave: their callers wait.
//  3. StartJob, oldest job first, in the rest of the room. A queued job
//     that waits for an older job sharing one of its concurrency keys gets
//     none.
//
// Room that the starts leave unused goes to the probes and the other way
// round. The result lists the probe commands first, then the cancels, then
// the starts.
//
// StartJob and probe commands depend on what the worker's current process
// can run (link features, operating system), so they wait until the Hello
// of epoch is stored (Worker.InfoCurrent): until then the stored Info is
// the previous process's, maybe another version. Storing the Hello wakes
// the worker's long poll if a derivation was held back, and fails the
// probes the process cannot run (ProbeBroker.refuse; derivations repeat
// that for probes queued on the previous process's Info meanwhile).
func (s *Service) commands(ctx context.Context, sess *session, w workers.Worker, epoch string) ([]*syncrv1.Command, error) {
	js, err := s.jobs.Unfinished(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	if js, err = s.reconcileEpoch(ctx, w.ID, epoch, js); err != nil {
		return nil, err
	}
	var cancels []*syncrv1.Command
	for _, j := range js {
		if j.Status.Active() && j.CancelRequested() && !sess.cancelAcked[j.ID.String()] {
			cancels = append(cancels, cancelCommand(j.ID.String()))
		}
	}
	for _, id := range sess.orphans {
		if !sess.cancelAcked[id] {
			cancels = append(cancels, cancelCommand(id))
		}
	}
	cancels = cancels[:min(len(cancels), s.cfg.MaxCommands)]
	if !w.InfoCurrent() {
		sess.awaitsHello = true
		return cancels, nil
	}
	if s.probes != nil {
		// Storing the Hello failed the probes this process cannot run; this
		// also catches one queued meanwhile on the previous process's Info.
		s.probes.refuse(w.ID, w.Name, w.Info)
	}
	room := s.cfg.MaxCommands - len(cancels)
	if room == 0 {
		return cancels, nil
	}
	var probes []*syncrv1.Command
	if s.probes != nil && w.Info.HasFeatures(link.FeatureProbe) {
		// Every run_probe derived here counts as sent, also one the
		// budget below leaves out (see ProbeBroker.commands).
		probes = s.probes.commands(w.ID, room)
	}
	startRoom := room - min(len(probes), room/2)
	if sess.stopping {
		startRoom = 0 // it would refuse them; the jobs wait for its next process
	}
	starts, err := s.starts(ctx, js, w.Info, startRoom)
	if err != nil {
		if len(cancels) == 0 && len(probes) == 0 {
			return nil, err
		}
		// A cancel or a probe (already counted as sent) must not wait for
		// another job's spec: send them, and let the next poll, which
		// follows their acks, retry the starts.
		s.log.WarnContext(ctx, "deriving the worker's job starts failed; sending its other commands", "worker_id", w.ID.String(), "error", err.Error())
		starts = nil
	}
	probes = probes[:min(len(probes), room-len(starts))]
	return slices.Concat(probes, cancels, starts), nil
}

// starts returns the StartJob commands of the queued jobs of js, oldest
// first, at most limit, for a worker whose current process reported info.
// A queued job that waits for an older job sharing one of its concurrency
// keys gets none.
func (s *Service) starts(ctx context.Context, js []jobs.Job, info workers.Info, limit int) ([]*syncrv1.Command, error) {
	if limit <= 0 {
		return nil, nil
	}
	blocked, err := s.blocked(ctx, js)
	if err != nil {
		return nil, err
	}
	var cmds []*syncrv1.Command
	for _, j := range js {
		if len(cmds) >= limit {
			break
		}
		if j.Status != domain.JobQueued {
			continue
		}
		if _, waits := blocked[j.ID]; waits {
			continue // not the head of its concurrency keys yet
		}
		// Invariant (package jobs): a queued job never has a cancel
		// request, so no StartJob follows a cancel.
		spec, err := s.startSpec(ctx, j, info)
		if err != nil {
			return nil, err
		}
		if spec == nil {
			continue // undeliverable; the job was failed
		}
		cmds = append(cmds, &syncrv1.Command{
			Id:      startPrefix + j.ID.String(),
			Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: j.ID.String(), Spec: spec}},
		})
	}
	return cmds, nil
}

// blocked returns the queued jobs of js that wait for an older unfinished
// job sharing one of their concurrency keys, with that job (the
// concurrency gate: only the oldest unfinished job of a key may start).
// It is decided from the stored jobs alone, so every controller and every
// poll agree.
func (s *Service) blocked(ctx context.Context, js []jobs.Job) (map[domain.JobID]domain.JobID, error) {
	var keyed []domain.JobID
	for _, j := range js {
		if j.Status == domain.JobQueued && len(j.ConcurrencyKeys) > 0 {
			keyed = append(keyed, j.ID)
		}
	}
	out := make(map[domain.JobID]domain.JobID)
	for chunk := range slices.Chunk(keyed, jobs.MaxBlockerIDs) {
		m, err := s.jobs.Blockers(ctx, chunk)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, m)
	}
	return out, nil
}

// startSpec builds a queued job's spec (see SpecBuilder). A job whose spec
// can never be built (a remote is gone, an invalid definition, unusable
// credentials) fails at once; startSpec then returns nil without an error.
// Other errors are returned, so the poll fails and the next one retries.
// The spec is only ever sent, never logged.
func (s *Service) startSpec(ctx context.Context, j jobs.Job, info workers.Info) (*syncrv1.JobSpec, error) {
	if s.specs == nil {
		return nil, s.failUnbuildable(ctx, j, jobs.CodeInvalidSpec, "the controller cannot build job specs yet",
			errors.New("no spec builder is configured"))
	}
	spec, err := s.specs.Build(ctx, j, info)
	var wrongOS *WrongOSError
	var missingHooks *MissingHooksError
	var sourceMethod *SourceMethodError
	switch {
	case err == nil:
		return spec, nil
	case errors.As(err, &sourceMethod):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeToolUnavailable, sourceMethod.Message(s.workerName(ctx, j)), err)
	case errors.As(err, &wrongOS):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeInvalidSpec, s.wrongOSMessage(ctx, j, wrongOS), err)
	case errors.As(err, &missingHooks):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeInvalidSpec, missingHooks.Message(s.workerName(ctx, j)), err)
	case errors.Is(err, errRepositoryUnavailable):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeRepositoryUnavailable, "the repository was deleted or is not available to its owner", err)
	case errors.Is(err, ErrWorkerOutdated):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeWorkerOutdated, s.outdatedMessage(ctx, j, err), err)
	case errors.Is(err, domain.ErrNotFound):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeRemoteUnavailable, "a remote of the job was deleted or is not available to its owner", err)
	case errors.Is(err, domain.ErrInvalidArgument):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeInvalidSpec, "the job's definition cannot be turned into a spec", err)
	case errors.Is(err, domain.ErrFailedPrecondition):
		return nil, s.failUnbuildable(ctx, j, jobs.CodeInternal, "the credentials of a remote of the job cannot be used", err)
	default:
		return nil, fmt.Errorf("build the spec of job %s: %w", j.ID, err)
	}
}

// outdatedMessage names the job's worker in the worker_outdated failure
// cause, and what it cannot run: the job's kind, or only its time limit or
// its hooks (a job of its kind without them would run).
func (s *Service) outdatedMessage(ctx context.Context, j jobs.Job, cause error) string {
	what := j.Kind + " jobs"
	switch {
	case errors.Is(cause, ErrTimeLimitUnsupported):
		what = "jobs with a time limit"
	case errors.Is(cause, ErrHooksUnsupported):
		what = "jobs with hooks"
	case errors.Is(cause, ErrRemoteSourceUnsupported):
		what = "backups of a folder of a remote"
	}
	if name := s.workerName(ctx, j); name != "" {
		return fmt.Sprintf("worker %q must be upgraded to run %s", name, what)
	}
	return fmt.Sprintf("the worker must be upgraded to run %s", what)
}

// workerName returns the name of j's worker, or "" if it cannot be read.
func (s *Service) workerName(ctx context.Context, j jobs.Job) string {
	w, err := s.workers.Get(ctx, j.WorkerID)
	if err != nil {
		return ""
	}
	return w.Name
}

// wrongOSMessage says why a job fails on a worker that runs another
// operating system than its definition needs.
func (s *Service) wrongOSMessage(ctx context.Context, j jobs.Job, e *WrongOSError) string {
	runs := cmp.Or(e.OS, "an unknown operating system")
	w, err := s.workers.Get(ctx, j.WorkerID)
	if err != nil {
		return fmt.Sprintf("%s; the worker runs %s", e.Required.Explain(), runs)
	}
	return fmt.Sprintf("%s; worker %q runs %s", e.Required.Explain(), w.Name, runs)
}

// failUnbuildable fails a queued job whose spec cannot be built. The error
// is logged, never sent anywhere: it may name internals.
func (s *Service) failUnbuildable(ctx context.Context, j jobs.Job, code, msg string, cause error) error {
	s.log.WarnContext(ctx, "job spec cannot be built; failing the job", "job_id", j.ID.String(), "error_code", code, "error", cause.Error())
	_, _, err := s.jobs.FailQueued(ctx, j.ID, time.Time{}, jobs.JobError{Code: code, Message: msg})
	return err
}

func cancelCommand(jobID string) *syncrv1.Command {
	return &syncrv1.Command{
		Id:      cancelPrefix + jobID,
		Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{JobId: jobID}},
	}
}
