package dispatch

import (
	"context"
	"errors"
	"slices"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// reconcileEpoch applies the epoch rules to js (the worker's unfinished
// jobs): a job accepted by an epoch other than the current one was lost
// with its worker process. Running jobs become interrupted; dispatched jobs
// never started, so they are re-queued (or cancelled, if a cancel was
// requested). It returns the jobs that are still unfinished. Idempotent, and
// run whenever unfinished jobs are loaded, so an epoch change whose
// reconcile failed half-way is completed by the next call.
func (s *Service) reconcileEpoch(ctx context.Context, workerID domain.WorkerID, epoch string, js []jobs.Job) ([]jobs.Job, error) {
	out := make([]jobs.Job, 0, len(js))
	var errs []error
	for _, j := range js {
		if j.Status.Active() && j.Epoch != epoch {
			var (
				next jobs.Job
				err  error
			)
			if j.Status == domain.JobRunning {
				next, _, err = s.jobs.Interrupt(ctx, workerID, j.ID, j.Epoch, jobs.JobError{
					Code:    jobs.CodeWorkerRestarted,
					Message: "the worker restarted while the job was running; its outcome is unknown",
				})
			} else {
				next, _, err = s.jobs.Requeue(ctx, workerID, j.ID, j.Epoch)
			}
			if err != nil {
				errs = append(errs, err)
			} else {
				j = next
				s.log.InfoContext(ctx, "job reconciled after worker restart", "worker_id", workerID.String(),
					"job_id", j.ID.String(), "status", string(j.Status))
			}
		}
		if !j.Status.Terminal() {
			out = append(out, j)
		}
	}
	return out, errors.Join(errs...)
}

// reconcileSnapshot compares the worker's state snapshot with the
// controller's view; see the rules in the package documentation. cursor is
// the ingest cursor after the envelope's events; snapSeq is the seq the
// snapshot accounts for (0 if unknown).
func (s *Service) reconcileSnapshot(ctx context.Context, sess *session, workerID domain.WorkerID, epoch string, cursor, snapSeq int64, state *syncrv1.WorkerState) error {
	if state.GetOutboxSaturated() != sess.saturated {
		sess.saturated = state.GetOutboxSaturated()
		s.log.WarnContext(ctx, "worker outbox saturation changed", "worker_id", workerID.String(),
			"saturated", sess.saturated, "backlog", state.GetOutboxBacklog())
	}
	if snapSeq > 0 && snapSeq < cursor {
		// Stale: a delayed or retried request older than events already
		// stored. Its job list may predate those events.
		return nil
	}
	js, err := s.jobs.Unfinished(ctx, workerID)
	if err != nil {
		return err
	}
	if js, err = s.reconcileEpoch(ctx, workerID, epoch, js); err != nil {
		return err
	}
	unfinished := make(map[string]jobs.Job, len(js))
	for _, j := range js {
		unfinished[j.ID.String()] = j
	}

	var errs []error
	listed := make(map[string]bool, len(state.GetJobs()))
	var orphans []string
	for _, aj := range state.GetJobs() {
		id := aj.GetJobId()
		if id == "" || listed[id] {
			continue
		}
		listed[id] = true
		j, ok := unfinished[id]
		switch {
		case !ok:
			// Finished, unknown or another worker's job: stop it.
			orphans = append(orphans, id)
		case j.Status == domain.JobQueued:
			// The worker holds it, so it accepted it: an implicit ack.
			if _, _, err := s.jobs.MarkDispatched(ctx, workerID, j.ID, epoch); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// A snapshot that accounts for exactly the stored events is current: a
	// running job it does not list has neither a result nor a holder.
	if snapSeq > 0 && snapSeq == cursor {
		for _, j := range js {
			if j.Status == domain.JobRunning && j.Epoch == epoch && !listed[j.ID.String()] {
				_, _, err := s.jobs.Interrupt(ctx, workerID, j.ID, epoch, jobs.JobError{
					Code:    jobs.CodeWorkerLost,
					Message: "the worker no longer holds the job and reported no result",
				})
				errs = append(errs, err)
			}
		}
	}

	if !slices.Equal(orphans, sess.orphans) {
		if len(orphans) > 0 {
			s.log.InfoContext(ctx, "worker runs jobs the controller considers finished or unknown; canceling them",
				"worker_id", workerID.String(), "jobs", len(orphans))
		}
		woke := slices.ContainsFunc(orphans, func(id string) bool { return !slices.Contains(sess.orphans, id) })
		sess.orphans = orphans
		if woke {
			s.hub.Wake(workerID)
		}
	}
	// Forget cancel acks of jobs the worker no longer holds.
	for id := range sess.cancelAcked {
		if !listed[id] {
			if _, ok := unfinished[id]; !ok {
				delete(sess.cancelAcked, id)
			}
		}
	}
	return errors.Join(errs...)
}
