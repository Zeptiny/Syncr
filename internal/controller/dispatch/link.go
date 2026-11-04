package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
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
	sess, _, err := s.begin(ctx, workerID, epoch)
	if err != nil {
		return nil, err
	}
	defer sess.mu.Unlock()
	return s.commands(ctx, sess, workerID, epoch)
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
			_, err = s.jobs.RecordProgress(ctx, workerID, id, epoch, jobs.Sample{Time: at, Stats: statsFromProto(p.Progress.GetStats())})
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
		case strings.HasPrefix(cmdID, startPrefix):
			id, err := domain.ParseJobID(strings.TrimPrefix(cmdID, startPrefix))
			if err != nil {
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

// commands implements Commands with the session locked.
func (s *Service) commands(ctx context.Context, sess *session, workerID domain.WorkerID, epoch string) ([]*syncrv1.Command, error) {
	js, err := s.jobs.Unfinished(ctx, workerID)
	if err != nil {
		return nil, err
	}
	if js, err = s.reconcileEpoch(ctx, workerID, epoch, js); err != nil {
		return nil, err
	}
	var cmds []*syncrv1.Command
	for _, j := range js {
		if len(cmds) >= s.cfg.MaxCommands {
			return cmds, nil
		}
		switch {
		case j.Status == domain.JobQueued:
			// Invariant (package jobs): a queued job never has a cancel
			// request, so no StartJob follows a cancel.
			spec, err := s.startSpec(ctx, j)
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
		case j.Status.Active() && j.CancelRequested() && !sess.cancelAcked[j.ID.String()]:
			cmds = append(cmds, cancelCommand(j.ID.String()))
		}
	}
	for _, id := range sess.orphans {
		if len(cmds) >= s.cfg.MaxCommands {
			break
		}
		if !sess.cancelAcked[id] {
			cmds = append(cmds, cancelCommand(id))
		}
	}
	return cmds, nil
}

// startSpec decrypts and decodes a queued job's spec. A spec that cannot be
// read (wrong master key, corruption) can never be delivered, so the job
// fails at once; startSpec then returns nil without an error.
func (s *Service) startSpec(ctx context.Context, j jobs.Job) (*syncrv1.JobSpec, error) {
	raw, err := s.jobs.Spec(j)
	spec := new(syncrv1.JobSpec)
	if err == nil {
		err = proto.Unmarshal(raw, spec)
	}
	if err == nil {
		return spec, nil
	}
	s.log.ErrorContext(ctx, "job spec is unreadable; failing the job", "job_id", j.ID.String(), "error", err.Error())
	_, _, ferr := s.jobs.FailQueued(ctx, j.ID, time.Time{}, jobs.JobError{Code: jobs.CodeInternal, Message: "the job's stored spec cannot be read"})
	return nil, ferr
}

func cancelCommand(jobID string) *syncrv1.Command {
	return &syncrv1.Command{
		Id:      cancelPrefix + jobID,
		Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{JobId: jobID}},
	}
}
