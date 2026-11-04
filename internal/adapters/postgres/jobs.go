package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateJob implements jobs.Repository. The insert is guarded by a
// WHERE EXISTS on the worker in the same statement, so a missing or removed
// worker inserts nothing and reports ErrNotFound.
func (s *Store) CreateJob(ctx context.Context, j jobs.Job) error {
	op := fmt.Sprintf("create job %s", j.ID)
	n, err := s.q.CreateJob(ctx, db.CreateJobParams{
		ID:                pgID(j.ID),
		WorkerID:          pgID(j.WorkerID),
		Kind:              j.Kind,
		Status:            string(j.Status),
		SpecEnc:           j.SpecEnc,
		Epoch:             j.Epoch,
		CancelRequestedAt: pgOptTime(j.CancelRequestedAt),
		CreatedAt:         pgTime(j.CreatedAt),
		QueuedAt:          pgTime(j.QueuedAt),
		DispatchedAt:      pgOptTime(j.DispatchedAt),
		StartedAt:         pgOptTime(j.StartedAt),
		FinishedAt:        pgOptTime(j.FinishedAt),
		Requeues:          int32(j.Requeues), //nolint:gosec // G115: a requeue count, bounded by the retry policy
		StatBytes:         j.Stats.Bytes,
		StatTotalBytes:    j.Stats.TotalBytes,
		StatFiles:         j.Stats.Files,
		StatTotalFiles:    j.Stats.TotalFiles,
		StatChecks:        j.Stats.Checks,
		StatDeletes:       j.Stats.Deletes,
		StatRenames:       j.Stats.Renames,
		StatErrors:        j.Stats.Errors,
		StatElapsedNs:     int64(j.Stats.Elapsed),
		ErrorCode:         cleanText(j.Error.Code),
		ErrorMessage:      cleanText(j.Error.Message),
		Warnings:          textArray(cleanTexts(j.Warnings)),
		ExitCode:          int32(j.ExitCode), //nolint:gosec // G115: a process exit code
		Output:            j.Output,
		Version:           j.Version,
	})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: worker %s: %w", op, j.WorkerID, domain.ErrNotFound)
	}
	return nil
}

// GetJob implements jobs.Repository.
func (s *Store) GetJob(ctx context.Context, id domain.JobID) (jobs.Job, error) {
	row, err := s.q.GetJob(ctx, pgID(id))
	if err != nil {
		return jobs.Job{}, mapErr(fmt.Sprintf("get job %s", id), err)
	}
	return jobFrom(row)
}

// UpdateJob implements jobs.Repository: UPDATE jobs SET ..., version =
// version + 1 WHERE id = $1 AND version = $2 RETURNING *. When it matches
// nothing, a second query tells a missing job (ErrNotFound) from a version
// mismatch (ErrConflict).
func (s *Store) UpdateJob(ctx context.Context, j jobs.Job) (jobs.Job, error) {
	op := fmt.Sprintf("update job %s", j.ID)
	row, err := s.q.UpdateJob(ctx, db.UpdateJobParams{
		ID:                pgID(j.ID),
		ExpectedVersion:   j.Version,
		Status:            string(j.Status),
		Epoch:             j.Epoch,
		CancelRequestedAt: pgOptTime(j.CancelRequestedAt),
		QueuedAt:          pgTime(j.QueuedAt),
		DispatchedAt:      pgOptTime(j.DispatchedAt),
		StartedAt:         pgOptTime(j.StartedAt),
		FinishedAt:        pgOptTime(j.FinishedAt),
		Requeues:          int32(j.Requeues), //nolint:gosec // G115: a requeue count, bounded by the retry policy
		StatBytes:         j.Stats.Bytes,
		StatTotalBytes:    j.Stats.TotalBytes,
		StatFiles:         j.Stats.Files,
		StatTotalFiles:    j.Stats.TotalFiles,
		StatChecks:        j.Stats.Checks,
		StatDeletes:       j.Stats.Deletes,
		StatRenames:       j.Stats.Renames,
		StatErrors:        j.Stats.Errors,
		StatElapsedNs:     int64(j.Stats.Elapsed),
		ErrorCode:         cleanText(j.Error.Code),
		ErrorMessage:      cleanText(j.Error.Message),
		Warnings:          textArray(cleanTexts(j.Warnings)),
		ExitCode:          int32(j.ExitCode), //nolint:gosec // G115: a process exit code
		Output:            j.Output,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		exists, qerr := s.q.JobExists(ctx, pgID(j.ID))
		if qerr != nil {
			return jobs.Job{}, mapErr(op, qerr)
		}
		if !exists {
			return jobs.Job{}, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return jobs.Job{}, fmt.Errorf("%s: not at version %d: %w", op, j.Version, domain.ErrConflict)
	}
	if err != nil {
		return jobs.Job{}, mapErr(op, err)
	}
	return jobFrom(row)
}

// ListJobs implements jobs.Repository. Zero-valued filter fields are passed
// as NULL, which the query treats as "no filter".
func (s *Store) ListJobs(ctx context.Context, f jobs.ListFilter) ([]jobs.Job, error) {
	statuses := make([]string, len(f.Statuses))
	for i, st := range f.Statuses {
		statuses[i] = string(st)
	}
	rows, err := s.q.ListJobs(ctx, db.ListJobsParams{
		WorkerID: pgOptID(f.WorkerID),
		Statuses: statuses,
		Before:   pgOptID(f.Before),
		RowLimit: limitArg(f.Limit),
	})
	if err != nil {
		return nil, mapErr("list jobs", err)
	}
	return jobsFrom(rows)
}

// ListUnfinishedJobs implements jobs.Repository.
func (s *Store) ListUnfinishedJobs(ctx context.Context, workerID domain.WorkerID) ([]jobs.Job, error) {
	rows, err := s.q.ListUnfinishedJobs(ctx, pgID(workerID))
	if err != nil {
		return nil, mapErr(fmt.Sprintf("list unfinished jobs of worker %s", workerID), err)
	}
	return jobsFrom(rows)
}

// ListQueuedJobs implements jobs.Repository.
func (s *Store) ListQueuedJobs(ctx context.Context, before time.Time, limit int) ([]jobs.Job, error) {
	rows, err := s.q.ListQueuedJobs(ctx, db.ListQueuedJobsParams{Before: pgTime(before), RowLimit: limitArg(limit)})
	if err != nil {
		return nil, mapErr("list queued jobs", err)
	}
	return jobsFrom(rows)
}
