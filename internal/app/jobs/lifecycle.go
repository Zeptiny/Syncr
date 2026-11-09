package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Link use cases, called by the dispatcher (internal/controller/dispatch).
// workerID is the authenticated worker; a job of another worker is reported
// as ErrNotFound. epoch is the worker process epoch the information comes
// from. Each call returns the job after the call and whether it changed it;
// a call whose precondition does not hold is a no-op, not an error.

// Unfinished returns the worker's queued, dispatched and running jobs,
// oldest first.
func (s *Service) Unfinished(ctx context.Context, workerID domain.WorkerID) ([]Job, error) {
	js, err := s.repo.ListUnfinishedJobs(ctx, workerID)
	if err != nil {
		return nil, fmt.Errorf("unfinished jobs of worker %s: %w", workerID, err)
	}
	return js, nil
}

// QueuedBefore returns up to limit jobs (any worker) queued since before
// `before`, oldest first.
func (s *Service) QueuedBefore(ctx context.Context, before time.Time, limit int) ([]Job, error) {
	js, err := s.repo.ListQueuedJobs(ctx, before, limit)
	if err != nil {
		return nil, fmt.Errorf("queued jobs: %w", err)
	}
	return js, nil
}

// MarkDispatched records that epoch accepted the job: queued -> dispatched.
// No-op unless the job is queued.
func (s *Service) MarkDispatched(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string) (Job, bool, error) {
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		if j.Status != domain.JobQueued {
			return false, nil
		}
		if err := setStatus(j, domain.JobDispatched); err != nil {
			return false, err
		}
		j.Epoch = epoch
		j.DispatchedAt = now
		return true, nil
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("mark job %s dispatched: %w", id, err)
	}
	return j, changed, nil
}

// MarkRunning records the worker's JobStarted: dispatched (by epoch) ->
// running, with startedAt (worker clock; zero means now). A queued job is
// first marked dispatched by epoch (the start proves acceptance).
func (s *Service) MarkRunning(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, startedAt time.Time) (Job, bool, error) {
	if _, _, err := s.MarkDispatched(ctx, workerID, id, epoch); err != nil {
		return Job{}, false, err
	}
	if startedAt.IsZero() {
		startedAt = s.clock.Now()
	}
	j, changed, err := s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		if j.Status != domain.JobDispatched || j.Epoch != epoch {
			return false, nil
		}
		if err := setStatus(j, domain.JobRunning); err != nil {
			return false, err
		}
		j.StartedAt = startedAt
		return true, nil
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("mark job %s running: %w", id, err)
	}
	return j, changed, nil
}

// RecordProgress publishes a live sample of a job running in epoch and
// stores it if Config.SampleInterval has passed since the last stored one.
// It reports whether the sample was accepted (the job runs in epoch).
func (s *Service) RecordProgress(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, smp Sample) (bool, error) {
	j, err := s.repo.GetJob(ctx, id)
	if err != nil {
		return false, fmt.Errorf("record progress of job %s: %w", id, err)
	}
	if j.WorkerID != workerID {
		return false, fmt.Errorf("record progress of job %s: job of another worker: %w", id, domain.ErrNotFound)
	}
	if j.Status != domain.JobRunning || j.Epoch != epoch {
		return false, nil
	}
	smp.JobID = id
	if smp.Time.IsZero() {
		smp.Time = s.clock.Now()
	}
	s.broker.Publish(Update{JobID: id, Progress: &smp})
	if !s.due(id, smp.Time) {
		return true, nil
	}
	if err := s.samples.AppendJobSample(ctx, smp); err != nil {
		return true, fmt.Errorf("record progress of job %s: %w", id, err)
	}
	s.stored(id, smp.Time)
	return true, nil
}

// Finish records a worker-reported outcome of a job accepted by epoch: a
// queued, or dispatched or running (by epoch) job moves to o.Status (which
// must be terminal) with the outcome's details. The final counters are
// also stored as the job's last sample.
//
// A cancelled outcome counts as cancelled only if a cancel was requested.
// Otherwise the worker stopped the job on its own (it is shutting down):
// a job that never started (still dispatched, so no JobStarted came before
// the result, and the result has no start time) goes back to the queue
// like the jobs a restarted worker never started (Requeue): nothing ran, so
// it does not finish and nobody is notified. A job that started is
// recorded as interrupted with code worker_shutdown. The decision is made
// atomically with the update, so a concurrent Cancel cannot be missed.
func (s *Service) Finish(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, o Outcome) (Job, bool, error) {
	if !o.Status.Terminal() {
		return Job{}, false, fmt.Errorf("finish job %s: %w: status %q is not terminal", id, domain.ErrInvalidArgument, o.Status)
	}
	if _, _, err := s.MarkDispatched(ctx, workerID, id, epoch); err != nil {
		return Job{}, false, err
	}
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		if !j.Status.Active() || j.Epoch != epoch {
			return false, nil
		}
		to, e := o.Status, o.Error
		if to == domain.JobCancelled {
			switch {
			case j.CancelRequested():
				if e.IsZero() || e.Code == CodeUnspecified {
					e.Code = CodeCancelled
				}
			case j.Status == domain.JobDispatched && o.StartedAt.IsZero():
				return true, requeue(j, now)
			default:
				to = domain.JobInterrupted
				e.Code = CodeWorkerShutdown
				if e.Message == "" {
					e.Message = "the worker stopped the job without a cancel request"
				}
			}
		}
		if err := setStatus(j, to); err != nil {
			return false, err
		}
		if j.StartedAt.IsZero() {
			j.StartedAt = o.StartedAt
		}
		j.FinishedAt = o.FinishedAt
		if j.FinishedAt.IsZero() {
			j.FinishedAt = now
		}
		j.Stats, j.Error, j.Warnings, j.ExitCode, j.Output = o.Stats, e, o.Warnings, o.ExitCode, o.Output
		return true, nil
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("finish job %s: %w", id, err)
	}
	if changed && j.Status == domain.JobQueued {
		s.log.InfoContext(ctx, "job re-queued: the worker stopped without starting it", "job_id", id.String(),
			"worker_id", workerID.String(), "requeues", j.Requeues)
		return j, changed, nil
	}
	if changed && !j.StartedAt.IsZero() {
		final := Sample{JobID: id, Time: j.FinishedAt, Stats: j.Stats}
		if err := s.samples.AppendJobSample(ctx, final); err != nil {
			s.log.WarnContext(ctx, "storing final sample failed", "job_id", id.String(), "error", err.Error())
		}
	}
	return j, changed, nil
}

// Requeue returns a job that epoch accepted but never started to the queue
// (its worker process was replaced): dispatched (by epoch) -> queued. A job
// with a cancel request is cancelled instead, so no StartJob is ever sent
// after a cancel.
func (s *Service) Requeue(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string) (Job, bool, error) {
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		if j.Status != domain.JobDispatched || j.Epoch != epoch {
			return false, nil
		}
		if j.CancelRequested() {
			return true, finish(j, domain.JobCancelled, now, JobError{Code: CodeCancelled, Message: "cancelled before the worker started it"})
		}
		return true, requeue(j, now)
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("requeue job %s: %w", id, err)
	}
	return j, changed, nil
}

// Interrupt ends a job that epoch accepted (dispatched or running) as
// interrupted with reason e.
func (s *Service) Interrupt(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, e JobError) (Job, bool, error) {
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		if !j.Status.Active() || j.Epoch != epoch {
			return false, nil
		}
		return true, finish(j, domain.JobInterrupted, now, e)
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("interrupt job %s: %w", id, err)
	}
	return j, changed, nil
}

// msgCancelUnconfirmed is the error message of a job whose cancel request
// could not be delivered because its worker was out of contact
// (CancelUnconfirmed, Abandon).
const msgCancelUnconfirmed = "cancelled while the worker was out of contact; the worker is told to stop the job if it comes back"

// CancelUnconfirmed ends a job that epoch accepted (dispatched or running)
// and whose cancel was requested as cancelled, without the worker's
// confirmation: its worker has been out of contact since the request, so
// the CancelJob cannot be delivered. A job without a cancel request is left
// alone. If the worker comes back still holding the job, the dispatcher
// tells it to stop (the job is finished as far as the controller knows),
// and the worker's late events and result change nothing.
func (s *Service) CancelUnconfirmed(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string) (Job, bool, error) {
	j, changed, err := s.abandon(ctx, workerID, id, epoch, nil)
	if err != nil {
		return Job{}, false, fmt.Errorf("cancel job %s without its worker: %w", id, err)
	}
	return j, changed, nil
}

// Abandon ends a job that epoch accepted (dispatched or running) on a
// worker that has been out of contact for too long (lost): a job with a
// cancel request becomes cancelled as in CancelUnconfirmed, any other job
// interrupted with reason e. As for CancelUnconfirmed, a worker that comes
// back is told to stop the job and its late events change nothing.
func (s *Service) Abandon(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, e JobError) (Job, bool, error) {
	j, changed, err := s.abandon(ctx, workerID, id, epoch, &e)
	if err != nil {
		return Job{}, false, fmt.Errorf("abandon job %s: %w", id, err)
	}
	return j, changed, nil
}

// abandon implements CancelUnconfirmed (lost == nil) and Abandon. The
// cancel request is checked atomically with the update, so a cancel
// requested concurrently is not missed.
func (s *Service) abandon(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, lost *JobError) (Job, bool, error) {
	now := s.clock.Now()
	return s.change(ctx, id, workerID, func(j *Job) (bool, error) {
		switch {
		case !j.Status.Active() || j.Epoch != epoch:
			return false, nil
		case j.CancelRequested():
			return true, finish(j, domain.JobCancelled, now, JobError{Code: CodeCancelled, Message: msgCancelUnconfirmed})
		case lost != nil:
			return true, finish(j, domain.JobInterrupted, now, *lost)
		}
		return false, nil
	})
}

// FailQueued fails a job that is still queued and, unless queuedBefore is
// zero, has been queued since before queuedBefore (so a job re-queued in
// the meantime is spared).
func (s *Service) FailQueued(ctx context.Context, id domain.JobID, queuedBefore time.Time, e JobError) (Job, bool, error) {
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, domain.WorkerID{}, func(j *Job) (bool, error) {
		if j.Status != domain.JobQueued || !queuedBefore.IsZero() && !j.QueuedAt.Before(queuedBefore) {
			return false, nil
		}
		return true, finish(j, domain.JobFailed, now, e)
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("fail job %s: %w", id, err)
	}
	return j, changed, nil
}

// ReleaseWorkerJobs ends the unfinished jobs of a removed worker: queued
// jobs fail, dispatched and running ones are interrupted (code
// worker_removed). Idempotent; implements workers.JobReleaser.
func (s *Service) ReleaseWorkerJobs(ctx context.Context, workerID domain.WorkerID) error {
	js, err := s.Unfinished(ctx, workerID)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range js {
		e := JobError{Code: CodeWorkerRemoved, Message: "the worker was removed"}
		if j.Status == domain.JobQueued {
			_, _, err = s.FailQueued(ctx, j.ID, time.Time{}, e)
		} else {
			_, _, err = s.Interrupt(ctx, workerID, j.ID, j.Epoch, e)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// requeue moves a dispatched job that never started back to the queue at
// now: the dispatcher derives a StartJob for it again, and the dispatch
// timeout starts over.
func requeue(j *Job, now time.Time) error {
	if err := setStatus(j, domain.JobQueued); err != nil {
		return err
	}
	j.Epoch = ""
	j.QueuedAt = now
	j.DispatchedAt = time.Time{}
	j.Requeues++
	return nil
}

// The sample rate limiter (Service.lastStored).
//
// An entry only throttles storage: without one, the job's next sample is
// stored, which costs one extra row and nothing else (samples are
// idempotent by time). So an entry is needed only while its job runs and
// keeps reporting, and it is dropped when the job finishes (published) or,
// whatever else happens (a sample stored concurrently with the job's end,
// a job whose end this process never sees), once it has gone unused for
// staleSampleAfter. That bounds the map by the jobs reporting progress
// within that time.

// staleSampleAfter is how long an unused rate limiter entry is kept, at
// least twice Config.SampleInterval.
const staleSampleAfter = 10 * time.Minute

// lastSample is a rate limiter entry.
type lastSample struct {
	// at is the Time of the job's last stored sample (worker clock).
	at time.Time
	// used is when the entry was last used (controller clock).
	used time.Time
}

// due reports whether a sample taken at t should be stored.
func (s *Service) due(id domain.JobID, t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastStored[id]
	if ok {
		last.used = s.clock.Now()
		s.lastStored[id] = last
	}
	return !ok || t.Sub(last.at) >= s.cfg.SampleInterval
}

// stored records that the sample taken at t was stored, and drops stale
// entries at most once per staleSampleAfter.
func (s *Service) stored(id domain.JobID, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	last := s.lastStored[id]
	if t.After(last.at) {
		last.at = t
	}
	last.used = now
	s.lastStored[id] = last
	stale := max(staleSampleAfter, 2*s.cfg.SampleInterval)
	if now.Sub(s.lastPrune) < stale {
		return
	}
	s.lastPrune = now
	for id, e := range s.lastStored {
		if now.Sub(e.used) >= stale {
			delete(s.lastStored, id)
		}
	}
}

func (s *Service) forget(id domain.JobID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lastStored, id)
}
