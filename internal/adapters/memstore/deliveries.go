package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements notifications.Repository (the outbox) with the
// semantics of the Postgres schema (migration 00003): the notified_at
// marks of jobs and runs, the (contact_id, dedupe_key) unique index, the
// foreign keys and CHECK constraints of deliveries, and claims that never
// hand out a delivery twice (the store's mutex stands in for FOR UPDATE
// SKIP LOCKED).

func cloneDelivery(d notifications.Delivery) notifications.Delivery {
	d.Payload = slices.Clone(d.Payload)
	return d
}

// scheduleSubscribersLocked returns the enabled contacts linked to the
// schedule, with the links' events.
func (s *Store) scheduleSubscribersLocked(id domain.ScheduleID) []notifications.Subscriber {
	sc, ok := s.schedules[id]
	if !ok {
		return nil
	}
	var out []notifications.Subscriber
	for _, l := range sc.Contacts { // sorted by contact ID
		if c, ok := s.contacts[l.ContactID]; ok && c.Enabled {
			out = append(out, notifications.Subscriber{ContactID: l.ContactID, Events: slices.Clone(l.Events)})
		}
	}
	return out
}

// manualSubscribersLocked returns the owner's enabled contacts that
// subscribed to manual jobs, with their JobEvents, by contact ID.
func (s *Store) manualSubscribersLocked(owner domain.UserID) []notifications.Subscriber {
	var out []notifications.Subscriber
	for _, c := range s.contacts {
		if c.OwnerID == owner && c.Enabled && len(c.JobEvents) > 0 {
			out = append(out, notifications.Subscriber{ContactID: c.ID, Events: slices.Clone(c.JobEvents)})
		}
	}
	slices.SortFunc(out, func(a, b notifications.Subscriber) int { return compareIDs(a.ContactID, b.ContactID) })
	return out
}

// PendingJobOutcomes implements notifications.Repository.
func (s *Store) PendingJobOutcomes(_ context.Context, limit int) ([]notifications.JobOutcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var pending []domain.JobID
	for id, j := range s.jobs {
		if _, done := s.jobNotified[id]; !done && j.Status.Terminal() {
			pending = append(pending, id)
		}
	}
	slices.SortFunc(pending, func(a, b domain.JobID) int {
		return cmp.Or(s.jobs[a].FinishedAt.Compare(s.jobs[b].FinishedAt), compareIDs(a, b))
	})
	if limit >= 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	out := make([]notifications.JobOutcome, 0, len(pending))
	for _, id := range pending {
		j := s.jobs[id]
		o := notifications.JobOutcome{
			JobID: j.ID, OwnerID: j.OwnerID, Kind: j.Kind, Status: j.Status,
			WorkerID: j.WorkerID, WorkerName: s.workers[j.WorkerID].Name,
			ErrorCode: j.Error.Code, ErrorMessage: j.Error.Message,
			Bytes: j.Stats.Bytes, Files: j.Stats.Files, Errors: j.Stats.Errors,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		}
		if sc, ok := s.schedules[j.ScheduleID]; ok && !j.ScheduleID.IsZero() {
			o.ScheduleID, o.ScheduleName = sc.ID, sc.Name
			o.Subscribers = s.scheduleSubscribersLocked(sc.ID)
		} else {
			o.Subscribers = s.manualSubscribersLocked(j.OwnerID)
		}
		out = append(out, o)
	}
	return out, nil
}

// PendingRunOutcomes implements notifications.Repository.
func (s *Store) PendingRunOutcomes(_ context.Context, limit int) ([]notifications.RunOutcome, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var pending []schedules.Run
	for id, r := range s.runs {
		if _, done := s.runNotified[id]; !done && (r.Status == schedules.RunMissed || r.Status == schedules.RunFailed) {
			pending = append(pending, r)
		}
	}
	slices.SortFunc(pending, func(a, b schedules.Run) int {
		return cmp.Or(a.UpdatedAt.Compare(b.UpdatedAt), compareIDs(a.ID, b.ID))
	})
	if limit >= 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	out := make([]notifications.RunOutcome, 0, len(pending))
	for _, r := range pending {
		sc := s.schedules[r.ScheduleID]
		out = append(out, notifications.RunOutcome{
			RunID: r.ID, ScheduleID: r.ScheduleID, ScheduleName: sc.Name, OwnerID: sc.OwnerID,
			Status: string(r.Status), Reason: r.Reason, Message: r.Message,
			ScheduledFor: r.ScheduledFor, Coalesced: r.Coalesced, WorkerID: r.WorkerID, UpdatedAt: r.UpdatedAt,
			Subscribers: s.scheduleSubscribersLocked(r.ScheduleID),
		})
	}
	return out, nil
}

// checkDeliveryLocked enforces the CHECK constraints and foreign keys of a
// new delivery row (the contact is checked by the caller).
func (s *Store) checkDeliveryLocked(d notifications.Delivery) error {
	invalid := func(what string) error {
		return fmt.Errorf("delivery %s: %w: %s", d.ID, domain.ErrInvalidArgument, what)
	}
	switch {
	case d.Event == domain.EventTest || !d.Event.Valid():
		return invalid("event")
	case !d.Status.Valid():
		return invalid("status")
	case d.MaxAttempts < 1:
		return invalid("max attempts")
	case d.DedupeKey == "":
		return invalid("dedupe key")
	}
	if _, ok := s.users[d.OwnerID]; !ok {
		return fmt.Errorf("owner %s of delivery %s: %w", d.OwnerID, d.ID, domain.ErrNotFound)
	}
	if _, ok := s.jobs[d.JobID]; !ok && !d.JobID.IsZero() {
		return fmt.Errorf("job %s of delivery %s: %w", d.JobID, d.ID, domain.ErrNotFound)
	}
	if _, ok := s.schedules[d.ScheduleID]; !ok && !d.ScheduleID.IsZero() {
		return fmt.Errorf("schedule %s of delivery %s: %w", d.ScheduleID, d.ID, domain.ErrNotFound)
	}
	return nil
}

// commitLocked inserts the new deliveries of ds (skipping existing dedupe
// keys and deleted contacts) after checking all of them, so it changes
// nothing on error. It returns how many it inserted.
func (s *Store) commitLocked(ds []notifications.Delivery) (int, error) {
	type key struct {
		contact domain.ContactID
		dedupe  string
	}
	taken := make(map[key]bool, len(s.deliveries))
	for _, d := range s.deliveries {
		taken[key{d.ContactID, d.DedupeKey}] = true
	}
	var insert []notifications.Delivery
	for _, d := range ds {
		if _, ok := s.contacts[d.ContactID]; !ok {
			continue // deleted since it was read
		}
		if err := s.checkDeliveryLocked(d); err != nil {
			return 0, err
		}
		k := key{d.ContactID, d.DedupeKey}
		if taken[k] {
			continue
		}
		if _, ok := s.deliveries[d.ID]; ok || slices.ContainsFunc(insert, func(o notifications.Delivery) bool { return o.ID == d.ID }) {
			return 0, fmt.Errorf("delivery %s: %w", d.ID, domain.ErrAlreadyExists)
		}
		taken[k] = true
		insert = append(insert, cloneDelivery(d))
	}
	for _, d := range insert {
		s.deliveries[d.ID] = d
	}
	return len(insert), nil
}

// CommitJobOutcome implements notifications.Repository.
func (s *Store) CommitJobOutcome(_ context.Context, id domain.JobID, ds []notifications.Delivery, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return 0, fmt.Errorf("job %s: %w", id, domain.ErrNotFound)
	}
	n, err := s.commitLocked(ds)
	if err != nil {
		return 0, err
	}
	if _, done := s.jobNotified[id]; !done {
		s.jobNotified[id] = at
	}
	return n, nil
}

// CommitRunOutcome implements notifications.Repository.
func (s *Store) CommitRunOutcome(_ context.Context, id domain.ScheduleRunID, ds []notifications.Delivery, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[id]; !ok {
		return 0, fmt.Errorf("run %s: %w", id, domain.ErrNotFound)
	}
	n, err := s.commitLocked(ds)
	if err != nil {
		return 0, err
	}
	if _, done := s.runNotified[id]; !done {
		s.runNotified[id] = at
	}
	return n, nil
}

// ClaimDeliveries implements notifications.Repository.
func (s *Store) ClaimDeliveries(_ context.Context, now time.Time, lease time.Duration, limit int) ([]notifications.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []notifications.Delivery
	for _, d := range s.deliveries {
		if d.Status == notifications.StatusPending && !d.NextAttemptAt.After(now) ||
			d.Status == notifications.StatusSending && !d.LeaseUntil.After(now) {
			due = append(due, d)
		}
	}
	slices.SortFunc(due, func(a, b notifications.Delivery) int {
		return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), compareIDs(a.ID, b.ID))
	})
	if limit >= 0 && len(due) > limit {
		due = due[:limit]
	}
	out := make([]notifications.Delivery, 0, len(due))
	for _, d := range due {
		d.Status = notifications.StatusSending
		d.Attempts++
		d.LeaseUntil = now.Add(lease)
		d.LastAttemptAt = now
		s.deliveries[d.ID] = d
		out = append(out, cloneDelivery(d))
	}
	return out, nil
}

// CompleteDelivery implements notifications.Repository.
func (s *Store) CompleteDelivery(_ context.Context, c notifications.Completion) (notifications.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[c.ID]
	if !ok {
		return notifications.Delivery{}, fmt.Errorf("delivery %s: %w", c.ID, domain.ErrNotFound)
	}
	if d.Status != notifications.StatusSending || d.Attempts != c.Attempts {
		return notifications.Delivery{}, fmt.Errorf("delivery %s is %s after %d attempts, not sending after %d: %w",
			c.ID, d.Status, d.Attempts, c.Attempts, domain.ErrConflict)
	}
	switch c.Status {
	case notifications.StatusDelivered:
		d.DeliveredAt = c.At
	case notifications.StatusPending:
		d.NextAttemptAt = c.NextAttemptAt
	case notifications.StatusDead:
	default:
		return notifications.Delivery{}, fmt.Errorf("complete delivery %s: %w: status %q", c.ID, domain.ErrInvalidArgument, c.Status)
	}
	d.Status, d.LastError, d.LeaseUntil = c.Status, c.LastError, time.Time{}
	s.deliveries[c.ID] = d
	return cloneDelivery(d), nil
}

// GetDelivery implements notifications.Repository.
func (s *Store) GetDelivery(_ context.Context, id domain.DeliveryID) (notifications.Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.deliveries[id]
	if !ok {
		return notifications.Delivery{}, fmt.Errorf("delivery %s: %w", id, domain.ErrNotFound)
	}
	return cloneDelivery(d), nil
}

// ListDeliveries implements notifications.Repository.
func (s *Store) ListDeliveries(_ context.Context, f notifications.DeliveryFilter) ([]notifications.Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []notifications.Delivery
	for _, d := range s.deliveries {
		if (f.OwnerID.IsZero() || d.OwnerID == f.OwnerID) &&
			(f.ContactID.IsZero() || d.ContactID == f.ContactID) &&
			(len(f.Statuses) == 0 || slices.Contains(f.Statuses, d.Status)) &&
			(f.Before.IsZero() || compareIDs(d.ID, f.Before) < 0) {
			out = append(out, cloneDelivery(d))
		}
	}
	slices.SortFunc(out, func(a, b notifications.Delivery) int { return compareIDs(b.ID, a.ID) })
	if f.Limit >= 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// RetryDelivery implements notifications.Repository.
func (s *Store) RetryDelivery(_ context.Context, id domain.DeliveryID, at time.Time) (notifications.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[id]
	if !ok {
		return notifications.Delivery{}, fmt.Errorf("delivery %s: %w", id, domain.ErrNotFound)
	}
	if d.Status != notifications.StatusDead {
		return notifications.Delivery{}, fmt.Errorf("delivery %s is %s, not dead: %w", id, d.Status, domain.ErrFailedPrecondition)
	}
	d.Status, d.Attempts, d.NextAttemptAt, d.LeaseUntil = notifications.StatusPending, 0, at, time.Time{}
	s.deliveries[id] = d
	return cloneDelivery(d), nil
}

// PurgeDeliveries implements notifications.Repository.
func (s *Store) PurgeDeliveries(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, d := range s.deliveries {
		if (d.Status == notifications.StatusDelivered || d.Status == notifications.StatusDead) && d.CreatedAt.Before(before) {
			delete(s.deliveries, id)
			n++
		}
	}
	return n, nil
}
