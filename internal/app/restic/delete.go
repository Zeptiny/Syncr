package restic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Delete deletes one of actor's repositories.
//
// By default it only forgets the repository (a soft delete): the password
// and the snapshot cache are dropped and the data in the backend is kept.
// While the repository's data is being deleted this fails with
// ErrRepositoryDeleting, naming the job to cancel first.
//
// With o.DeleteData it also deletes restic's files in the backend:
// o.ConfirmName must equal the repository's name exactly (a
// *domain.ValidationError on "confirm_name" otherwise). It picks the
// worker (Deletions.Pick; a *domain.ValidationError on "worker_id" when
// the remote needs an explicit one), moves the repository to Deleting,
// which pauses its schedules, and queues the restic_delete_data job,
// whose ID and worker it returns. Nothing changes when the confirmation or
// the pick fails. The repository is deleted, and its password wiped, only
// when that job succeeds (FinishDeletions). Repeating the request while
// the deletion is pending returns the same job (without a worker), and
// queues the job again should it be missing.
//
// A transient failure to queue the job is returned and leaves the
// repository deleting: the same request resumes it. A permanent one
// (ErrNotFound, ErrFailedPrecondition) reverts the repository to active
// with that error as its DeletionError.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, o DeleteOptions) (DeleteResult, error) {
	const op = "delete repository"
	r, err := s.owned(ctx, actor.UserID, id)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("%s: %w", op, err)
	}
	if !o.DeleteData {
		if r.Deleting {
			return DeleteResult{}, deletingError(op, r)
		}
		err := s.repo.DeleteRepo(ctx, id, s.clock.Now())
		switch {
		case errors.Is(err, domain.ErrFailedPrecondition):
			// A deletion started meanwhile.
			if cur, gerr := s.repo.GetRepo(ctx, id); gerr == nil && cur.Deleting {
				return DeleteResult{}, deletingError(op, cur)
			}
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, ErrRepositoryDeleting)
		case err != nil:
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, err)
		}
		s.log.InfoContext(ctx, "repository forgotten", "repository_id", id.String(), "owner_id", r.OwnerID.String())
		return DeleteResult{}, nil
	}

	if o.ConfirmName != r.Name {
		return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, fieldError("confirm_name", "type the repository's name to confirm"))
	}
	if s.deletions == nil {
		return DeleteResult{}, fmt.Errorf("%s %s: %w: data deletion is not available", op, id, domain.ErrFailedPrecondition)
	}
	// Two rounds: a concurrent request may mark the repository between
	// the read and MarkDeleting; the second round then resumes its job.
	for range 2 {
		if r.Deleting {
			return s.resumeDeletion(ctx, op, r, o.WorkerID)
		}
		worker, err := s.deletions.Pick(ctx, r.OwnerID, r.RemoteID, o.WorkerID)
		if err != nil {
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, err)
		}
		jobID := s.ids.NewJobID()
		marked, err := s.repo.MarkDeleting(ctx, id, jobID, s.clock.Now())
		if errors.Is(err, domain.ErrConflict) {
			if r, err = s.owned(ctx, actor.UserID, id); err != nil {
				return DeleteResult{}, fmt.Errorf("%s: %w", op, err)
			}
			continue
		}
		if err != nil {
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, err)
		}
		s.log.InfoContext(ctx, "repository data deletion requested", "repository_id", id.String(), "owner_id", r.OwnerID.String(),
			"job_id", jobID.String(), "worker_id", worker.String())
		if err := s.startDeletion(ctx, marked, worker); err != nil {
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, id, err)
		}
		return DeleteResult{DeletionJobID: jobID, WorkerID: worker}, nil
	}
	return DeleteResult{}, fmt.Errorf("%s %s: %w: its deletion state keeps changing; try again", op, id, domain.ErrConflict)
}

// deletingError is the failure of "forget only" on a deleting repository.
func deletingError(op string, r Repo) error {
	return fmt.Errorf("%s %s: %w; cancel job %s first", op, r.ID, ErrRepositoryDeleting, r.DeletionJobID)
}

// resumeDeletion answers a repeated DeleteData request: it queues the
// reserved job again, in case an earlier attempt never created it, and
// returns it. The job may already run on another worker than the one
// picked now, so no worker is reported; when no worker can be picked the
// existing job is still reported (if it was never created, FinishDeletions
// gives the deletion up after DeletionGrace).
func (s *Service) resumeDeletion(ctx context.Context, op string, r Repo, workerID domain.WorkerID) (DeleteResult, error) {
	out := DeleteResult{DeletionJobID: r.DeletionJobID}
	worker, err := s.deletions.Pick(ctx, r.OwnerID, r.RemoteID, workerID)
	if err != nil {
		if ctx.Err() != nil {
			return DeleteResult{}, fmt.Errorf("%s %s: %w", op, r.ID, err)
		}
		return out, nil
	}
	if err := s.startDeletion(ctx, r, worker); err != nil {
		return DeleteResult{}, fmt.Errorf("%s %s: %w", op, r.ID, err)
	}
	return out, nil
}

// startDeletion queues the deletion job of the deleting repository r on
// worker. A permanent failure ends the deletion as failed.
func (s *Service) startDeletion(ctx context.Context, r Repo, worker domain.WorkerID) error {
	err := s.deletions.Start(ctx, r.OwnerID, r.ID, r.DeletionJobID, worker)
	if err == nil {
		return nil
	}
	var verr *domain.ValidationError
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrFailedPrecondition) && !errors.As(err, &verr) {
		return err // transient: the repository stays deleting, a retry resumes
	}
	o := DeletionOutcome{Error: clipRunes("the deletion could not be started: "+err.Error(), MaxDeletionErrorLen)}
	// The request's context may be gone; the revert must still happen.
	ferr := s.repo.FinishDeletion(context.WithoutCancel(ctx), r.ID, r.DeletionJobID, o, s.clock.Now())
	if ferr != nil && !errors.Is(ferr, domain.ErrConflict) {
		s.log.ErrorContext(ctx, "cannot revert a repository whose deletion did not start", "repository_id", r.ID.String(),
			"job_id", r.DeletionJobID.String(), "error", ferr.Error())
	}
	return err
}

// FinishDeletions completes the pending data deletions whose job has
// ended, or was never created within DeletionGrace: a succeeded job
// deletes the repository and wipes its password; anything else makes it
// active again with a DeletionError. It returns how many deletions it
// ended. Each is a compare-and-swap on the repository's deletion job, so
// competing controllers and repeated passes are harmless.
func (s *Service) FinishDeletions(ctx context.Context) (int, error) {
	now := s.clock.Now()
	pending, err := s.repo.ListPendingDeletions(ctx, now.Add(-DeletionGrace), FinishBatch)
	if err != nil {
		return 0, fmt.Errorf("finish deletions: %w", err)
	}
	var (
		done int
		errs []error
	)
	for _, p := range pending {
		o := deletionOutcome(p)
		err := s.repo.FinishDeletion(ctx, p.RepoID, p.JobID, o, now)
		switch {
		case errors.Is(err, domain.ErrConflict):
			continue // ended by another pass, or a newer attempt replaced the job
		case err != nil:
			errs = append(errs, fmt.Errorf("repository %s: %w", p.RepoID, err))
			continue
		}
		done++
		if o.Succeeded {
			s.log.InfoContext(ctx, "repository deleted with its data", "repository_id", p.RepoID.String(),
				"owner_id", p.OwnerID.String(), "job_id", p.JobID.String())
		} else {
			s.log.WarnContext(ctx, "repository data deletion failed", "repository_id", p.RepoID.String(),
				"owner_id", p.OwnerID.String(), "job_id", p.JobID.String(), "job_status", string(p.JobStatus), "reason", o.Error)
		}
	}
	if len(errs) > 0 {
		return done, fmt.Errorf("finish deletions: %w", errors.Join(errs...))
	}
	return done, nil
}

// deletionOutcome decides how a pending deletion ends. The job's error is
// already scrubbed by the worker.
func deletionOutcome(p PendingDeletion) DeletionOutcome {
	switch p.JobStatus {
	case domain.JobSucceeded:
		return DeletionOutcome{Succeeded: true}
	case domain.JobCancelled:
		return DeletionOutcome{Cancelled: true, Error: "the deletion was cancelled; some repository files may already be gone"}
	case "":
		return DeletionOutcome{Error: "the deletion job was never created; try again"}
	}
	msg := "deleting the data failed"
	switch {
	case p.JobErrorCode != "" && p.JobErrorMessage != "":
		msg += ": " + p.JobErrorCode + ": " + p.JobErrorMessage
	case p.JobErrorCode+p.JobErrorMessage != "":
		msg += ": " + p.JobErrorCode + p.JobErrorMessage
	default:
		msg += ": the job was " + string(p.JobStatus)
	}
	return DeletionOutcome{Error: clipRunes(msg, MaxDeletionErrorLen)}
}

// Run completes data deletions (FinishDeletions) every
// Config.FinishInterval and whenever Wake is called, until ctx is done.
// It returns nil then.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.FinishInterval)
	defer t.Stop()
	for {
		if _, err := s.FinishDeletions(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "completing repository deletions failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Wake asks Run for a pass soon (a restic_delete_data job finished). It
// never blocks.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
