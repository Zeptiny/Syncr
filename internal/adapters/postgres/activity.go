package postgres

// This file is the jobs.ActivityRepository of the contracts step: both
// methods return a wrapped errors.ErrUnsupported (the storetest suite
// skips) until the dashboard track implements them with
// queries/activity.sql.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ jobs.ActivityRepository = (*Store)(nil)

// errActivityPending is the error of the activity stubs.
func errActivityPending(op string) error {
	return fmt.Errorf("postgres: %s: %w (job activity is not queried from Postgres yet)", op, errors.ErrUnsupported)
}

// JobActivity implements jobs.ActivityRepository (stub).
func (s *Store) JobActivity(context.Context, jobs.ActivityQuery) ([]jobs.ActivityRow, error) {
	return nil, errActivityPending("job activity")
}

// CountUnfinished implements jobs.ActivityRepository (stub).
func (s *Store) CountUnfinished(context.Context, domain.UserID) (map[domain.JobStatus]int64, error) {
	return nil, errActivityPending("count unfinished jobs")
}
