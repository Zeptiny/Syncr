package postgres

// This file is the schedules.Repository of the contracts step: every method
// returns a wrapped errors.ErrUnsupported (the storetest suite skips) until
// the schedules track implements it with queries/schedules.sql.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ schedules.Repository = (*Store)(nil)

// errSchedulesPending is the error of the schedules stubs.
func errSchedulesPending(op string) error {
	return fmt.Errorf("postgres: %s: %w (schedules are not stored in Postgres yet)", op, errors.ErrUnsupported)
}

// CreateSchedule implements schedules.Repository (stub).
func (s *Store) CreateSchedule(context.Context, schedules.Schedule) error {
	return errSchedulesPending("create schedule")
}

// GetSchedule implements schedules.Repository (stub).
func (s *Store) GetSchedule(context.Context, domain.ScheduleID) (schedules.Schedule, error) {
	return schedules.Schedule{}, errSchedulesPending("get schedule")
}

// GetSchedules implements schedules.Repository (stub).
func (s *Store) GetSchedules(context.Context, []domain.ScheduleID) ([]schedules.Schedule, error) {
	return nil, errSchedulesPending("get schedules")
}

// ListSchedules implements schedules.Repository (stub).
func (s *Store) ListSchedules(context.Context, schedules.ListFilter) ([]schedules.Schedule, error) {
	return nil, errSchedulesPending("list schedules")
}

// UpdateSchedule implements schedules.Repository (stub).
func (s *Store) UpdateSchedule(context.Context, schedules.Schedule, bool) (schedules.Schedule, error) {
	return schedules.Schedule{}, errSchedulesPending("update schedule")
}

// SetScheduleEnabled implements schedules.Repository (stub).
func (s *Store) SetScheduleEnabled(context.Context, domain.ScheduleID, schedules.Toggle) (schedules.Schedule, error) {
	return schedules.Schedule{}, errSchedulesPending("set schedule enabled")
}

// DeleteSchedule implements schedules.Repository (stub).
func (s *Store) DeleteSchedule(context.Context, domain.ScheduleID) error {
	return errSchedulesPending("delete schedule")
}

// ListDueSchedules implements schedules.Repository (stub).
func (s *Store) ListDueSchedules(context.Context, time.Time, int) ([]schedules.Schedule, error) {
	return nil, errSchedulesPending("list due schedules")
}

// FireSchedule implements schedules.Repository (stub).
func (s *Store) FireSchedule(context.Context, schedules.Fire) (schedules.Run, error) {
	return schedules.Run{}, errSchedulesPending("fire schedule")
}

// AddRun implements schedules.Repository (stub).
func (s *Store) AddRun(context.Context, schedules.Run, bool) (schedules.Run, error) {
	return schedules.Run{}, errSchedulesPending("add run")
}

// GetRun implements schedules.Repository (stub).
func (s *Store) GetRun(context.Context, domain.ScheduleRunID) (schedules.Run, error) {
	return schedules.Run{}, errSchedulesPending("get run")
}

// ListOpenRuns implements schedules.Repository (stub).
func (s *Store) ListOpenRuns(context.Context, int) ([]schedules.Run, error) {
	return nil, errSchedulesPending("list open runs")
}

// UpdateRun implements schedules.Repository (stub).
func (s *Store) UpdateRun(context.Context, schedules.RunUpdate) (schedules.Run, error) {
	return schedules.Run{}, errSchedulesPending("update run")
}

// ListRuns implements schedules.Repository (stub).
func (s *Store) ListRuns(context.Context, schedules.RunFilter) ([]schedules.Run, error) {
	return nil, errSchedulesPending("list runs")
}

// CountRuns implements schedules.Repository (stub).
func (s *Store) CountRuns(context.Context, schedules.RunFilter) (int64, error) {
	return 0, errSchedulesPending("count runs")
}

// LatestRuns implements schedules.Repository (stub).
func (s *Store) LatestRuns(context.Context, []domain.ScheduleID) (map[domain.ScheduleID]schedules.Run, error) {
	return nil, errSchedulesPending("latest runs")
}

// HasActiveRun implements schedules.Repository (stub).
func (s *Store) HasActiveRun(context.Context, domain.ScheduleID) (bool, error) {
	return false, errSchedulesPending("has active run")
}

// ListUpcoming implements schedules.Repository (stub).
func (s *Store) ListUpcoming(context.Context, domain.UserID, int) ([]schedules.Upcoming, error) {
	return nil, errSchedulesPending("list upcoming")
}
