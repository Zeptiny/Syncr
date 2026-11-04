package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// sweepBatch is how many timed-out jobs ExpireQueued handles per query.
const sweepBatch = 100

// Run performs periodic housekeeping until ctx ends: every
// Config.SweepInterval it fails jobs past the dispatch timeout
// (ExpireQueued). Errors are logged; Run returns nil when ctx ends.
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
		}
	}
}

// ExpireQueued fails every job that has been queued for longer than
// Config.DispatchTimeout with code dispatch_timeout and a message saying
// why its worker did not take it. It returns how many jobs it failed.
func (s *Service) ExpireQueued(ctx context.Context) (int, error) {
	cutoff := s.clock.Now().Add(-s.cfg.DispatchTimeout)
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
	default:
		return prefix + fmt.Sprintf("worker %q is online but did not accept the job", w.Name)
	}
}
