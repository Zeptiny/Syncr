package postgres

import (
	"context"
	"fmt"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ jobs.ActivityRepository = (*Store)(nil)

// JobActivity implements jobs.ActivityRepository with one GROUP BY over
// date_trunc(unit, finished_at, tz) (queries/activity.sql), so local hours
// and days follow the zone's DST rules exactly as Postgres resolves them.
func (s *Store) JobActivity(ctx context.Context, q jobs.ActivityQuery) ([]jobs.ActivityRow, error) {
	const op = "job activity"
	if !q.Unit.Valid() || q.Location == nil {
		return nil, fmt.Errorf("%s: %w: unit %q or time zone missing", op, domain.ErrInvalidArgument, q.Unit)
	}
	rows, err := s.q.JobActivity(ctx, db.JobActivityParams{
		Unit:    string(q.Unit),
		Tz:      q.Location.String(),
		FromT:   pgTime(q.From),
		UntilT:  pgTime(q.Until),
		OwnerID: pgOptID(q.OwnerID),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]jobs.ActivityRow, len(rows))
	for i, r := range rows {
		if !r.Bucket.Valid {
			return nil, fmt.Errorf("%s: a bucket is NULL", op)
		}
		out[i] = jobs.ActivityRow{
			Bucket: r.Bucket.Time.UTC(),
			Status: domain.JobStatus(r.Status),
			Sums:   sums(r.Jobs, r.Bytes, r.Files, r.Started, r.RunUs, r.Dispatched, r.WaitUs),
		}
	}
	return out, nil
}

// sums builds jobs.Sums from a row's counts and microsecond sums.
func sums(n, bytes, files, started, runUs, dispatched, waitUs int64) jobs.Sums {
	return jobs.Sums{
		Jobs: n, Bytes: bytes, Files: files,
		Started: started, RunTimeMicros: runUs,
		Dispatched: dispatched, QueueWaitMicros: waitUs,
	}
}

// JobTotals implements jobs.ActivityRepository with one GROUP BY over the
// finished jobs (queries/activity.sql), an index-only scan of
// jobs_totals_idx.
func (s *Store) JobTotals(ctx context.Context, q jobs.TotalsQuery) ([]jobs.TotalsRow, error) {
	const op = "job totals"
	bounds := make([]int64, len(q.RunTimeBounds))
	for i, b := range q.RunTimeBounds {
		bounds[i] = b.Microseconds()
	}
	rows, err := s.q.JobTotals(ctx, db.JobTotalsParams{
		BoundsUs: bounds,
		FromT:    pgOptTime(q.From),
		UntilT:   pgOptTime(q.Until),
		OwnerID:  pgOptID(q.OwnerID),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]jobs.TotalsRow, len(rows))
	for i, r := range rows {
		var w domain.WorkerID
		if err := decodeID(&w, r.WorkerID); err != nil {
			return nil, fmt.Errorf("%s: worker ID: %w", op, err)
		}
		out[i] = jobs.TotalsRow{
			Kind: r.Kind, WorkerID: w, Status: domain.JobStatus(r.Status), ErrorCode: r.ErrorCode,
			RunTimeBucket: int(r.RunBucket),
			Sums:          sums(r.Jobs, r.Bytes, r.Files, r.Started, r.RunUs, r.Dispatched, r.WaitUs),
		}
	}
	return out, nil
}

// UnfinishedJobs implements jobs.ActivityRepository.
func (s *Store) UnfinishedJobs(ctx context.Context, owner domain.UserID) ([]jobs.UnfinishedRow, error) {
	const op = "unfinished jobs"
	rows, err := s.q.UnfinishedJobs(ctx, pgOptID(owner))
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]jobs.UnfinishedRow, len(rows))
	for i, r := range rows {
		var w domain.WorkerID
		if err := decodeID(&w, r.WorkerID); err != nil {
			return nil, fmt.Errorf("%s: worker ID: %w", op, err)
		}
		out[i] = jobs.UnfinishedRow{
			Status: domain.JobStatus(r.Status), Kind: r.Kind, WorkerID: w, Jobs: r.Jobs,
			OldestQueuedAt: timeFrom(r.OldestQueuedAt),
		}
	}
	return out, nil
}
