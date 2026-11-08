package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time check that Store implements the outbox port.
var _ notifications.Repository = (*Store)(nil)

// The outbox is the deliveries table plus the notified_at marks of jobs and
// schedule_runs.
//
//   - Commit*Outcome is one transaction: it locks the source row (FOR NO
//     KEY UPDATE, so concurrent commits of one outcome run one after the
//     other), inserts the deliveries (ON CONFLICT on the (contact_id,
//     dedupe_key) index DO NOTHING; skipped if the contact is gone, which
//     the insert checks under FOR KEY SHARE) and sets notified_at.
//   - ClaimDeliveries is one statement: the due rows are locked FOR UPDATE
//     SKIP LOCKED in a materialized CTE and updated to sending, so
//     concurrent claims (goroutines, pools, controllers) are disjoint.
//   - CompleteDelivery and RetryDelivery are conditional updates (sending
//     with the claimed attempt count; dead).

// PendingJobOutcomes implements notifications.Repository. The subscribers
// are loaded with one query per kind (schedule links, manual job events).
func (s *Store) PendingJobOutcomes(ctx context.Context, limit int) ([]notifications.JobOutcome, error) {
	const op = "pending job outcomes"
	rows, err := s.q.PendingJobOutcomes(ctx, limitArg(limit))
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]notifications.JobOutcome, 0, len(rows))
	var scheduleIDs, ownerIDs []pgtype.UUID
	for _, row := range rows {
		o := notifications.JobOutcome{
			Kind: row.Kind, Definition: row.Definition, Status: domain.JobStatus(row.Status), WorkerName: row.WorkerName,
			ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
			Bytes: row.StatBytes, Files: row.StatFiles, Errors: row.StatErrors,
			StartedAt: timeFrom(row.StartedAt), FinishedAt: timeFrom(row.FinishedAt),
		}
		if err := errors.Join(decodeID(&o.JobID, row.ID), decodeID(&o.OwnerID, row.OwnerID), decodeID(&o.WorkerID, row.WorkerID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if row.ScheduleName != nil { // the schedule exists: a scheduled job
			if err := decodeID(&o.ScheduleID, row.ScheduleID); err != nil {
				return nil, fmt.Errorf("%s: %w", op, err)
			}
			o.ScheduleName = *row.ScheduleName
			scheduleIDs = append(scheduleIDs, row.ScheduleID)
		} else {
			ownerIDs = append(ownerIDs, row.OwnerID)
		}
		out = append(out, o)
	}
	bySchedule, err := s.scheduleSubscribers(ctx, op, scheduleIDs)
	if err != nil {
		return nil, err
	}
	byOwner, err := s.manualSubscribers(ctx, op, ownerIDs)
	if err != nil {
		return nil, err
	}
	for i, o := range out {
		if o.ScheduleID.IsZero() {
			out[i].Subscribers = slices.Clone(byOwner[o.OwnerID])
		} else {
			out[i].Subscribers = slices.Clone(bySchedule[o.ScheduleID])
		}
	}
	return out, nil
}

// PendingRunOutcomes implements notifications.Repository.
func (s *Store) PendingRunOutcomes(ctx context.Context, limit int) ([]notifications.RunOutcome, error) {
	const op = "pending run outcomes"
	rows, err := s.q.PendingRunOutcomes(ctx, limitArg(limit))
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]notifications.RunOutcome, 0, len(rows))
	scheduleIDs := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		o := notifications.RunOutcome{
			ScheduleName: row.ScheduleName, Status: row.Status, Reason: row.Reason, Message: row.Message,
			ScheduledFor: timeFrom(row.ScheduledFor), Coalesced: int(row.Coalesced), UpdatedAt: timeFrom(row.UpdatedAt),
		}
		if err := errors.Join(decodeID(&o.RunID, row.ID), decodeID(&o.ScheduleID, row.ScheduleID),
			decodeID(&o.OwnerID, row.OwnerID), decodeID(&o.WorkerID, row.WorkerID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		scheduleIDs = append(scheduleIDs, row.ScheduleID)
		out = append(out, o)
	}
	bySchedule, err := s.scheduleSubscribers(ctx, op, scheduleIDs)
	if err != nil {
		return nil, err
	}
	for i, o := range out {
		out[i].Subscribers = slices.Clone(bySchedule[o.ScheduleID])
	}
	return out, nil
}

// scheduleSubscribers returns the enabled contacts linked to each of ids,
// with the links' events, by contact ID.
func (s *Store) scheduleSubscribers(ctx context.Context, op string, ids []pgtype.UUID) (map[domain.ScheduleID][]notifications.Subscriber, error) {
	out := map[domain.ScheduleID][]notifications.Subscriber{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.ScheduleSubscribers(ctx, ids)
	if err != nil {
		return nil, mapErr(op+": schedule subscribers", err)
	}
	for _, row := range rows {
		var id domain.ScheduleID
		sub := notifications.Subscriber{Events: outboxEvents(row.Events)}
		if err := errors.Join(decodeID(&id, row.ScheduleID), decodeID(&sub.ContactID, row.ContactID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out[id] = append(out[id], sub)
	}
	return out, nil
}

// manualSubscribers returns each of owners' enabled contacts with job
// events, with those events, by contact ID.
func (s *Store) manualSubscribers(ctx context.Context, op string, owners []pgtype.UUID) (map[domain.UserID][]notifications.Subscriber, error) {
	out := map[domain.UserID][]notifications.Subscriber{}
	if len(owners) == 0 {
		return out, nil
	}
	rows, err := s.q.ManualSubscribers(ctx, owners)
	if err != nil {
		return nil, mapErr(op+": manual job subscribers", err)
	}
	for _, row := range rows {
		var owner domain.UserID
		sub := notifications.Subscriber{Events: outboxEvents(row.JobEvents)}
		if err := errors.Join(decodeID(&owner, row.OwnerID), decodeID(&sub.ContactID, row.ContactID)); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out[owner] = append(out[owner], sub)
	}
	return out, nil
}

// CommitJobOutcome implements notifications.Repository.
func (s *Store) CommitJobOutcome(ctx context.Context, id domain.JobID, ds []notifications.Delivery, at time.Time) (int, error) {
	op := fmt.Sprintf("commit outcome of job %s", id)
	return s.commitOutcome(ctx, op, ds, func(q *db.Queries) error {
		_, err := q.LockJobOutcome(ctx, pgID(id))
		return err
	}, func(q *db.Queries) error {
		return q.MarkJobNotified(ctx, db.MarkJobNotifiedParams{ID: pgID(id), At: pgTime(at)})
	})
}

// CommitRunOutcome implements notifications.Repository.
func (s *Store) CommitRunOutcome(ctx context.Context, id domain.ScheduleRunID, ds []notifications.Delivery, at time.Time) (int, error) {
	op := fmt.Sprintf("commit outcome of run %s", id)
	return s.commitOutcome(ctx, op, ds, func(q *db.Queries) error {
		_, err := q.LockRunOutcome(ctx, pgID(id))
		return err
	}, func(q *db.Queries) error {
		return q.MarkRunNotified(ctx, db.MarkRunNotifiedParams{ID: pgID(id), At: pgTime(at)})
	})
}

// commitOutcome locks a source row (no row: ErrNotFound), inserts ds and
// marks the source notified, in one transaction. It returns how many
// deliveries it inserted.
func (s *Store) commitOutcome(ctx context.Context, op string, ds []notifications.Delivery, lock, mark func(*db.Queries) error) (int, error) {
	params := make([]db.InsertDeliveryParams, len(ds))
	for i, d := range ds {
		p, err := insertDeliveryParams(d)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", op, err)
		}
		params[i] = p
	}
	inserted := 0
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		if err := lock(q); err != nil {
			return mapErr(op, err)
		}
		for i, p := range params {
			n, err := q.InsertDelivery(ctx, p)
			if err != nil {
				return mapErr(fmt.Sprintf("%s: delivery %s", op, ds[i].ID), err)
			}
			inserted += int(n)
		}
		return mapErr(op, mark(q))
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// insertDeliveryParams converts a new delivery. The CHECK constraints
// enforce the event, status and attempt limit; the dedupe key is checked
// here.
func insertDeliveryParams(d notifications.Delivery) (db.InsertDeliveryParams, error) {
	if d.DedupeKey == "" {
		return db.InsertDeliveryParams{}, fmt.Errorf("delivery %s: %w: empty dedupe key", d.ID, domain.ErrInvalidArgument)
	}
	attempts, err := int32Of(d.Attempts)
	if err != nil {
		return db.InsertDeliveryParams{}, fmt.Errorf("delivery %s: attempts: %w", d.ID, err)
	}
	maxAttempts, err := int32Of(d.MaxAttempts)
	if err != nil {
		return db.InsertDeliveryParams{}, fmt.Errorf("delivery %s: max attempts: %w", d.ID, err)
	}
	return db.InsertDeliveryParams{
		ID:            pgID(d.ID),
		ContactID:     pgID(d.ContactID),
		OwnerID:       pgID(d.OwnerID),
		Event:         string(d.Event),
		DedupeKey:     cleanText(d.DedupeKey),
		JobID:         pgOptID(d.JobID),
		ScheduleID:    pgOptID(d.ScheduleID),
		Payload:       d.Payload,
		Status:        string(d.Status),
		Attempts:      attempts,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: pgTime(d.NextAttemptAt),
		LeaseUntil:    pgOptTime(d.LeaseUntil),
		LastAttemptAt: pgOptTime(d.LastAttemptAt),
		DeliveredAt:   pgOptTime(d.DeliveredAt),
		LastError:     cleanText(d.LastError),
		CreatedAt:     pgTime(d.CreatedAt),
	}, nil
}

// int32Of converts n to an integer column value: ErrInvalidArgument if it
// does not fit.
func int32Of(n int) (int32, error) {
	if n < -1<<31 || n > 1<<31-1 {
		return 0, fmt.Errorf("%w: %d is out of range", domain.ErrInvalidArgument, n)
	}
	return int32(n), nil
}

// ClaimDeliveries implements notifications.Repository (one statement; see
// queries/deliveries.sql).
func (s *Store) ClaimDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]notifications.Delivery, error) {
	const op = "claim deliveries"
	rows, err := s.q.ClaimDeliveries(ctx, db.ClaimDeliveriesParams{
		Now: pgTime(now), LeaseUntil: pgTime(now.Add(lease)), RowLimit: limitArg(limit),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out, err := deliveriesFrom(op, rows)
	if err != nil {
		return nil, err
	}
	// RETURNING has no order.
	slices.SortFunc(out, func(a, b notifications.Delivery) int {
		return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), cmp.Compare(a.ID.String(), b.ID.String()))
	})
	return out, nil
}

// CompleteDelivery implements notifications.Repository: UPDATE ... WHERE id
// AND sending AND attempts RETURNING *; when it matches nothing, a second
// query tells a missing delivery (ErrNotFound) from a stale claim
// (ErrConflict).
func (s *Store) CompleteDelivery(ctx context.Context, c notifications.Completion) (notifications.Delivery, error) {
	op := fmt.Sprintf("complete delivery %s", c.ID)
	switch c.Status {
	case notifications.StatusDelivered, notifications.StatusPending, notifications.StatusDead:
	default:
		return notifications.Delivery{}, fmt.Errorf("%s: %w: status %q", op, domain.ErrInvalidArgument, c.Status)
	}
	attempts, err := int32Of(c.Attempts)
	if err != nil {
		return notifications.Delivery{}, fmt.Errorf("%s: %w", op, err)
	}
	row, err := s.q.CompleteDelivery(ctx, db.CompleteDeliveryParams{
		ID: pgID(c.ID), Attempts: attempts, Status: string(c.Status), LastError: cleanText(c.LastError),
		At: pgTime(c.At), NextAttemptAt: pgTime(c.NextAttemptAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return notifications.Delivery{}, s.missingOr(ctx, op, c.ID,
			fmt.Errorf("%s: not sending after %d attempts: %w", op, c.Attempts, domain.ErrConflict))
	}
	if err != nil {
		return notifications.Delivery{}, mapErr(op, err)
	}
	return deliveryFrom(op, row)
}

// missingOr returns ErrNotFound if delivery id does not exist, else
// otherwise.
func (s *Store) missingOr(ctx context.Context, op string, id domain.DeliveryID, otherwise error) error {
	found, err := s.q.DeliveryExists(ctx, pgID(id))
	switch {
	case err != nil:
		return mapErr(op, err)
	case !found:
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return otherwise
}

// GetDelivery implements notifications.Repository.
func (s *Store) GetDelivery(ctx context.Context, id domain.DeliveryID) (notifications.Delivery, error) {
	op := fmt.Sprintf("get delivery %s", id)
	row, err := s.q.GetDelivery(ctx, pgID(id))
	if err != nil {
		return notifications.Delivery{}, mapErr(op, err)
	}
	return deliveryFrom(op, row)
}

// ListDeliveries implements notifications.Repository.
func (s *Store) ListDeliveries(ctx context.Context, f notifications.DeliveryFilter) ([]notifications.Delivery, error) {
	const op = "list deliveries"
	statuses := make([]string, len(f.Statuses))
	for i, st := range f.Statuses {
		statuses[i] = string(st)
	}
	rows, err := s.q.ListDeliveries(ctx, db.ListDeliveriesParams{
		OwnerID: pgOptID(f.OwnerID), ContactID: pgOptID(f.ContactID), Statuses: statuses,
		Before: pgOptID(f.Before), RowLimit: limitArg(f.Limit),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	return deliveriesFrom(op, rows)
}

// RetryDelivery implements notifications.Repository.
func (s *Store) RetryDelivery(ctx context.Context, id domain.DeliveryID, at time.Time) (notifications.Delivery, error) {
	op := fmt.Sprintf("retry delivery %s", id)
	row, err := s.q.RetryDelivery(ctx, db.RetryDeliveryParams{ID: pgID(id), At: pgTime(at)})
	if errors.Is(err, pgx.ErrNoRows) {
		return notifications.Delivery{}, s.missingOr(ctx, op, id, fmt.Errorf("%s: not dead: %w", op, domain.ErrFailedPrecondition))
	}
	if err != nil {
		return notifications.Delivery{}, mapErr(op, err)
	}
	return deliveryFrom(op, row)
}

// PurgeDeliveries implements notifications.Repository.
func (s *Store) PurgeDeliveries(ctx context.Context, before time.Time) (int, error) {
	n, err := s.q.PurgeDeliveries(ctx, pgTime(before))
	if err != nil {
		return 0, mapErr("purge deliveries", err)
	}
	return int(n), nil
}

func deliveriesFrom(op string, rows []db.Delivery) ([]notifications.Delivery, error) {
	out := make([]notifications.Delivery, 0, len(rows))
	for _, row := range rows {
		d, err := deliveryFrom(op, row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// deliveryFrom converts a row.
func deliveryFrom(op string, row db.Delivery) (notifications.Delivery, error) {
	d := notifications.Delivery{
		Event:         domain.NotificationEvent(row.Event),
		DedupeKey:     row.DedupeKey,
		Payload:       row.Payload,
		Status:        notifications.Status(row.Status),
		Attempts:      int(row.Attempts),
		MaxAttempts:   int(row.MaxAttempts),
		NextAttemptAt: timeFrom(row.NextAttemptAt),
		LeaseUntil:    timeFrom(row.LeaseUntil),
		LastAttemptAt: timeFrom(row.LastAttemptAt),
		DeliveredAt:   timeFrom(row.DeliveredAt),
		LastError:     row.LastError,
		CreatedAt:     timeFrom(row.CreatedAt),
	}
	if err := errors.Join(decodeID(&d.ID, row.ID), decodeID(&d.ContactID, row.ContactID), decodeID(&d.OwnerID, row.OwnerID),
		decodeID(&d.JobID, row.JobID), decodeID(&d.ScheduleID, row.ScheduleID)); err != nil {
		return notifications.Delivery{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}
