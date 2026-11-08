package postgres

// This file is the notifications.Repository (the outbox) of the contracts
// step: every method returns a wrapped errors.ErrUnsupported (the storetest
// suite skips) until the notifications track implements it with
// queries/deliveries.sql.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ notifications.Repository = (*Store)(nil)

// errDeliveriesPending is the error of the outbox stubs.
func errDeliveriesPending(op string) error {
	return fmt.Errorf("postgres: %s: %w (the notification outbox is not stored in Postgres yet)", op, errors.ErrUnsupported)
}

// PendingJobOutcomes implements notifications.Repository (stub).
func (s *Store) PendingJobOutcomes(context.Context, int) ([]notifications.JobOutcome, error) {
	return nil, errDeliveriesPending("pending job outcomes")
}

// PendingRunOutcomes implements notifications.Repository (stub).
func (s *Store) PendingRunOutcomes(context.Context, int) ([]notifications.RunOutcome, error) {
	return nil, errDeliveriesPending("pending run outcomes")
}

// CommitJobOutcome implements notifications.Repository (stub).
func (s *Store) CommitJobOutcome(context.Context, domain.JobID, []notifications.Delivery, time.Time) (int, error) {
	return 0, errDeliveriesPending("commit job outcome")
}

// CommitRunOutcome implements notifications.Repository (stub).
func (s *Store) CommitRunOutcome(context.Context, domain.ScheduleRunID, []notifications.Delivery, time.Time) (int, error) {
	return 0, errDeliveriesPending("commit run outcome")
}

// ClaimDeliveries implements notifications.Repository (stub).
func (s *Store) ClaimDeliveries(context.Context, time.Time, time.Duration, int) ([]notifications.Delivery, error) {
	return nil, errDeliveriesPending("claim deliveries")
}

// CompleteDelivery implements notifications.Repository (stub).
func (s *Store) CompleteDelivery(context.Context, notifications.Completion) (notifications.Delivery, error) {
	return notifications.Delivery{}, errDeliveriesPending("complete delivery")
}

// GetDelivery implements notifications.Repository (stub).
func (s *Store) GetDelivery(context.Context, domain.DeliveryID) (notifications.Delivery, error) {
	return notifications.Delivery{}, errDeliveriesPending("get delivery")
}

// ListDeliveries implements notifications.Repository (stub).
func (s *Store) ListDeliveries(context.Context, notifications.DeliveryFilter) ([]notifications.Delivery, error) {
	return nil, errDeliveriesPending("list deliveries")
}

// RetryDelivery implements notifications.Repository (stub).
func (s *Store) RetryDelivery(context.Context, domain.DeliveryID, time.Time) (notifications.Delivery, error) {
	return notifications.Delivery{}, errDeliveriesPending("retry delivery")
}

// PurgeDeliveries implements notifications.Repository (stub).
func (s *Store) PurgeDeliveries(context.Context, time.Time) (int, error) {
	return 0, errDeliveriesPending("purge deliveries")
}
