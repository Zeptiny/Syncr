package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateJob implements jobs.Repository. The job and its remotes are
// inserted in one transaction. The job insert is guarded by a WHERE EXISTS
// on the worker in the same statement, so a missing or removed worker
// inserts nothing and reports ErrNotFound; a missing owner, remote or
// schedule violates a foreign key (ErrNotFound) and rolls everything back.
func (s *Store) CreateJob(ctx context.Context, j jobs.Job) error {
	op := fmt.Sprintf("create job %s", j.ID)
	// Errors are mapped once, below: pgx errors keep their SQLSTATE through
	// the wrapping.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		n, err := q.CreateJob(ctx, createJobParams(j))
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("worker %s: %w", j.WorkerID, domain.ErrNotFound)
		}
		for _, id := range j.RemoteIDs {
			if err := q.InsertJobRemote(ctx, db.InsertJobRemoteParams{JobID: pgID(j.ID), RemoteID: pgID(id)}); err != nil {
				return fmt.Errorf("remote %s: %w", id, err)
			}
		}
		return nil
	})
	return mapErr(op, err)
}

func createJobParams(j jobs.Job) db.CreateJobParams {
	return db.CreateJobParams{
		ID:                pgID(j.ID),
		WorkerID:          pgID(j.WorkerID),
		OwnerID:           pgID(j.OwnerID),
		Kind:              j.Kind,
		Status:            string(j.Status),
		Definition:        j.Definition,
		SearchText:        cleanText(j.SearchText),
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
		ScheduleID:        pgOptID(j.ScheduleID),
		RepositoryID:      pgOptID(j.RepositoryID),
		ConcurrencyKeys:   textArray(j.ConcurrencyKeys),
		TimeLimitNs:       int64(j.TimeLimit),
	}
}

// jobsWithRemotes converts rows and fills in their RemoteIDs with one
// query. The remote rows never change after the job's insert, so reading
// them in a separate statement is consistent.
func (s *Store) jobsWithRemotes(ctx context.Context, op string, rows []db.Job) ([]jobs.Job, error) {
	out, err := jobsFrom(rows)
	if err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]pgtype.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	refs, err := s.q.GetJobRemoteIDs(ctx, ids)
	if err != nil {
		return nil, mapErr(op+": remotes", err)
	}
	byJob, err := remoteIDsByJob(refs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].RemoteIDs = byJob[pgID(out[i].ID)]
	}
	return out, nil
}

// jobWithRemotes is jobsWithRemotes for one row.
func (s *Store) jobWithRemotes(ctx context.Context, op string, row db.Job) (jobs.Job, error) {
	out, err := s.jobsWithRemotes(ctx, op, []db.Job{row})
	if err != nil {
		return jobs.Job{}, err
	}
	return out[0], nil
}

// GetJob implements jobs.Repository.
func (s *Store) GetJob(ctx context.Context, id domain.JobID) (jobs.Job, error) {
	op := fmt.Sprintf("get job %s", id)
	row, err := s.q.GetJob(ctx, pgID(id))
	if err != nil {
		return jobs.Job{}, mapErr(op, err)
	}
	return s.jobWithRemotes(ctx, op, row)
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
		TimeLimitNs:       int64(j.TimeLimit),
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
	return s.jobWithRemotes(ctx, op, row)
}

// ListJobs implements jobs.Repository. Zero-valued filter fields are passed
// as NULL (or an empty array), which the query treats as "no filter". The
// query text is escaped for LIKE, so it matches as a plain substring.
func (s *Store) ListJobs(ctx context.Context, f jobs.ListFilter) ([]jobs.Job, error) {
	statuses := make([]string, len(f.Statuses))
	for i, st := range f.Statuses {
		statuses[i] = string(st)
	}
	var query *string
	if f.Query != "" {
		q := likeEscape(cleanText(f.Query))
		query = &q
	}
	rows, err := s.q.ListJobs(ctx, db.ListJobsParams{
		OwnerID:       pgOptID(f.OwnerID),
		WorkerID:      pgOptID(f.WorkerID),
		Statuses:      statuses,
		Kinds:         textArray(f.Kinds),
		RemoteID:      pgOptID(f.RemoteID),
		ScheduleID:    pgOptID(f.ScheduleID),
		RepositoryID:  pgOptID(f.RepositoryID),
		Query:         query,
		CreatedAfter:  pgOptTime(f.CreatedAfter),
		CreatedBefore: pgOptTime(f.CreatedBefore),
		Before:        pgOptID(f.Before),
		RowLimit:      limitArg(f.Limit),
	})
	if err != nil {
		return nil, mapErr("list jobs", err)
	}
	return s.jobsWithRemotes(ctx, "list jobs", rows)
}

// ListUnfinishedJobs implements jobs.Repository.
func (s *Store) ListUnfinishedJobs(ctx context.Context, workerID domain.WorkerID) ([]jobs.Job, error) {
	op := fmt.Sprintf("list unfinished jobs of worker %s", workerID)
	rows, err := s.q.ListUnfinishedJobs(ctx, pgID(workerID))
	if err != nil {
		return nil, mapErr(op, err)
	}
	return s.jobsWithRemotes(ctx, op, rows)
}

// ListQueuedJobs implements jobs.Repository. The query is key-aware (see
// jobs.Repository): a keyed job waiting for an older sharer is no
// candidate.
func (s *Store) ListQueuedJobs(ctx context.Context, before time.Time, limit int) ([]jobs.Job, error) {
	rows, err := s.q.ListQueuedJobs(ctx, db.ListQueuedJobsParams{Before: pgTime(before), RowLimit: limitArg(limit)})
	if err != nil {
		return nil, mapErr("list queued jobs", err)
	}
	return s.jobsWithRemotes(ctx, "list queued jobs", rows)
}

// ListJobBlockers implements jobs.Repository (query ListJobBlockers).
func (s *Store) ListJobBlockers(ctx context.Context, ids []domain.JobID) (map[domain.JobID]domain.JobID, error) {
	out := make(map[domain.JobID]domain.JobID)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.ListJobBlockers(ctx, pgIDs(ids))
	if err != nil {
		return nil, mapErr("list job blockers", err)
	}
	for _, r := range rows {
		var job, blocker domain.JobID
		if err := errors.Join(decodeID(&job, r.JobID), decodeID(&blocker, r.BlockerID)); err != nil {
			return nil, fmt.Errorf("list job blockers: %w", err)
		}
		out[job] = blocker
	}
	return out, nil
}

// ListWorkersWaitingOnKeys implements jobs.Repository (query
// ListWorkersWaitingOnKeys).
func (s *Store) ListWorkersWaitingOnKeys(ctx context.Context, keys []string) ([]domain.WorkerID, error) {
	out := []domain.WorkerID{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.q.ListWorkersWaitingOnKeys(ctx, keys)
	if err != nil {
		return nil, mapErr("list workers waiting on keys", err)
	}
	for _, r := range rows {
		var id domain.WorkerID
		if err := decodeID(&id, r); err != nil {
			return nil, fmt.Errorf("list workers waiting on keys: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}
