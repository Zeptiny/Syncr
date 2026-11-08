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
			Jobs:   r.Jobs,
			Bytes:  r.Bytes,
			Files:  r.Files,
		}
	}
	return out, nil
}

// CountUnfinished implements jobs.ActivityRepository.
func (s *Store) CountUnfinished(ctx context.Context, owner domain.UserID) (map[domain.JobStatus]int64, error) {
	rows, err := s.q.CountUnfinishedJobs(ctx, pgOptID(owner))
	if err != nil {
		return nil, mapErr("count unfinished jobs", err)
	}
	out := make(map[domain.JobStatus]int64, len(rows))
	for _, r := range rows {
		out[domain.JobStatus(r.Status)] = r.Jobs
	}
	return out, nil
}
