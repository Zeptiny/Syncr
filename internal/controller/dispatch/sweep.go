package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// sweepBatch is how many timed-out jobs ExpireQueued handles per query.
const sweepBatch = 100

// Run performs periodic housekeeping until ctx ends: every
// Config.SweepInterval it fails jobs past the dispatch timeout
// (ExpireQueued) and ends the jobs of workers out of contact for too long
// (ExpireLost). Errors are logged; Run returns nil when ctx ends.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := s.ExpireQueued(ctx); err != nil && ctx.Err() == nil {
				s.log.ErrorContext(ctx, "dispatch timeout sweep failed", "error", err.Error())
			}
			if _, err := s.ExpireLost(ctx); err != nil && ctx.Err() == nil {
				s.log.ErrorContext(ctx, "lost worker sweep failed", "error", err.Error())
			}
		}
	}
}

// ExpireLost ends the dispatched and running jobs of workers that have been
// out of contact (no link call) for too long, and returns how many it
// ended. A worker's silence is counted from its last contact, or from this
// Service's start if that is later (workers cannot reach a controller that
// is down, and they reconnect within seconds of its start):
//
//   - silent for Config.CancelGrace: a job whose cancel was requested at
//     least CancelGrace ago becomes cancelled (CancelUnconfirmed), since
//     the CancelJob cannot be delivered;
//   - silent for Config.LostAfter: every job ends (Abandon), as cancelled if
//     a cancel was requested and as interrupted (worker_lost) otherwise.
//
// A worker that comes back later is told to stop the jobs it still holds
// (the snapshot reconcile's orphans), and its late events for them are
// acknowledged without effect. Removed workers are left to
// workers.Service.Remove.
func (s *Service) ExpireLost(ctx context.Context) (int, error) {
	ws, err := s.workers.List(ctx, workers.ListFilter{})
	if err != nil {
		return 0, err
	}
	ended := 0
	var errs []error
	for _, w := range ws {
		if w.Link.Epoch == "" || s.silence(w) < s.cfg.CancelGrace {
			continue // never connected (no accepted jobs), or in contact
		}
		n, err := s.expireLost(ctx, w.ID)
		ended += n
		errs = append(errs, err)
	}
	return ended, errors.Join(errs...)
}

// silence is how long w has been out of contact as far as this Service can
// tell.
func (s *Service) silence(w workers.Worker) time.Duration {
	return s.clock.Now().Sub(maxTime(w.Link.LastSeen, s.started))
}

// expireLost applies ExpireLost to one worker. It holds the worker's
// session, so it never races the worker's own link calls, and re-reads the
// worker under it: a call that got in first counts as contact.
func (s *Service) expireLost(ctx context.Context, id domain.WorkerID) (int, error) {
	sess := s.session(id)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	w, err := s.workers.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return 0, nil // removed meanwhile; Remove ends its jobs
	}
	if err != nil {
		return 0, err
	}
	silent := s.silence(w)
	if silent < s.cfg.CancelGrace {
		return 0, nil
	}
	js, err := s.jobs.Unfinished(ctx, id)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()
	lostMsg := fmt.Sprintf("worker %q has been out of contact for more than %v (last seen %s); the job's outcome is unknown",
		w.Name, s.cfg.LostAfter, w.Link.LastSeen.Format(time.RFC3339))
	ended := 0
	var errs []error
	for _, j := range js {
		var (
			done    jobs.Job
			changed bool
			err     error
		)
		switch {
		case !j.Status.Active():
			continue // queued jobs time out by the dispatch timeout
		case silent >= s.cfg.LostAfter:
			done, changed, err = s.jobs.Abandon(ctx, id, j.ID, j.Epoch, jobs.JobError{Code: jobs.CodeWorkerLost, Message: lostMsg})
		case j.CancelRequested() && now.Sub(j.CancelRequestedAt) >= s.cfg.CancelGrace:
			done, changed, err = s.jobs.CancelUnconfirmed(ctx, id, j.ID, j.Epoch)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if changed {
			ended++
			s.log.WarnContext(ctx, "job of a worker out of contact ended", "job_id", j.ID.String(), "worker_id", id.String(),
				"status", string(done.Status), "last_seen", w.Link.LastSeen.Format(time.RFC3339))
		}
	}
	return ended, errors.Join(errs...)
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ExpireQueued fails every job that has been queued for longer than
// Config.DispatchTimeout with code dispatch_timeout and a message saying
// why its worker did not take it. It returns how many jobs it failed. A
// job's time in the queue is counted from when it was queued, or from this
// Service's start if that is later: while the controller is down no worker
// can take a job, and workers reconnect within seconds of its start.
func (s *Service) ExpireQueued(ctx context.Context) (int, error) {
	cutoff := s.clock.Now().Add(-s.cfg.DispatchTimeout)
	if !s.started.Before(cutoff) {
		return 0, nil // no job has waited that long for this controller
	}
	failed := 0
	for {
		js, err := s.jobs.QueuedBefore(ctx, cutoff, sweepBatch)
		if err != nil {
			return failed, err
		}
		n := 0
		for _, j := range js {
			e := jobs.JobError{Code: jobs.CodeDispatchTimeout, Message: s.timeoutMessage(ctx, j)}
			_, changed, err := s.jobs.FailQueued(ctx, j.ID, cutoff, e)
			if err != nil {
				return failed, err
			}
			if changed {
				n++
				s.log.WarnContext(ctx, "job not dispatched in time", "job_id", j.ID.String(),
					"worker_id", j.WorkerID.String(), "timeout", s.cfg.DispatchTimeout.String())
			}
		}
		failed += n
		if len(js) < sweepBatch || n == 0 {
			return failed, nil
		}
	}
}

func (s *Service) timeoutMessage(ctx context.Context, j jobs.Job) string {
	prefix := fmt.Sprintf("not dispatched within %v: ", s.cfg.DispatchTimeout)
	w, err := s.workers.Get(ctx, j.WorkerID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return prefix + "the worker no longer exists"
	case err != nil:
		return prefix + "the worker did not accept the job"
	}
	switch s.workers.Status(w) {
	case domain.WorkerPending:
		return prefix + fmt.Sprintf("worker %q has never connected", w.Name)
	case domain.WorkerOffline:
		return prefix + fmt.Sprintf("worker %q is offline (last seen %s)", w.Name, w.Link.LastSeen.Format(time.RFC3339))
	}
	if !w.InfoCurrent() {
		// Its jobs wait for its Hello (see commands).
		return prefix + fmt.Sprintf("worker %q is online but its process has not reported what it can run", w.Name)
	}
	return prefix + fmt.Sprintf("worker %q is online but did not accept the job", w.Name)
}
