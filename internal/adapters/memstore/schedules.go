package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file implements schedules.Repository with the semantics of the
// Postgres schema (migration 00003): the foreign keys of schedules,
// schedule_remotes and schedule_contacts, the CHECK constraints, the
// case-insensitive name index and the one-open-run index of schedule_runs.

func cloneSchedule(s schedules.Schedule) schedules.Schedule {
	s.Definition = slices.Clone(s.Definition)
	s.RemoteIDs = slices.Clone(s.RemoteIDs)
	s.Contacts = slices.Clone(s.Contacts)
	for i := range s.Contacts {
		s.Contacts[i].Events = slices.Clone(s.Contacts[i].Events)
	}
	return s
}

// checkScheduleLocked enforces the CHECK constraints and foreign keys of a
// schedule row and its links (the owner is checked by the caller), and
// normalizes the links as the tables store them: remotes as a set by ID,
// contacts by ID (a contact linked twice violates the primary key).
func (s *Store) checkScheduleLocked(sc *schedules.Schedule) error {
	invalid := func(what string) error {
		return fmt.Errorf("schedule %s: %w: %s", sc.ID, domain.ErrInvalidArgument, what)
	}
	switch n := utf8.RuneCountInString(sc.Name); {
	case n < 1 || n > schedules.MaxNameLen:
		return invalid("name length")
	case sc.Target.WorkerID.IsZero() == (sc.Target.Label == ""):
		return invalid("exactly one of worker and label")
	case !sc.Missed.Action.Valid():
		return invalid("missed action")
	case sc.Missed.MaxDelay < 0:
		return invalid("max delay")
	case !sc.Overlap.Valid():
		return invalid("overlap")
	case !sc.Enabled && !sc.NextRunAt.IsZero():
		return invalid("a disabled schedule has a next run")
	case sc.Version < 1:
		return invalid("version")
	}
	if !sc.Target.WorkerID.IsZero() {
		if _, ok := s.workers[sc.Target.WorkerID]; !ok {
			return fmt.Errorf("worker %s of schedule %s: %w", sc.Target.WorkerID, sc.ID, domain.ErrNotFound)
		}
	}
	for _, id := range sc.RemoteIDs {
		if _, ok := s.remotes[id]; !ok {
			return fmt.Errorf("remote %s of schedule %s: %w", id, sc.ID, domain.ErrNotFound)
		}
	}
	for _, l := range sc.Contacts {
		if _, ok := s.contacts[l.ContactID]; !ok {
			return fmt.Errorf("contact %s of schedule %s: %w", l.ContactID, sc.ID, domain.ErrNotFound)
		}
		if len(l.Events) == 0 || !l.Events.Within(domain.ScheduleEvents()) {
			return invalid("contact events")
		}
	}
	*sc = cloneSchedule(*sc)
	slices.SortFunc(sc.RemoteIDs, compareIDs)
	sc.RemoteIDs = slices.Compact(sc.RemoteIDs)
	slices.SortFunc(sc.Contacts, func(a, b schedules.ContactLink) int { return compareIDs(a.ContactID, b.ContactID) })
	for i := 1; i < len(sc.Contacts); i++ {
		if sc.Contacts[i].ContactID == sc.Contacts[i-1].ContactID {
			return fmt.Errorf("contact %s linked twice to schedule %s: %w", sc.Contacts[i].ContactID, sc.ID, domain.ErrAlreadyExists)
		}
	}
	return nil
}

// scheduleNameFreeLocked returns ErrAlreadyExists if another schedule of
// owner has name, case-insensitively.
func (s *Store) scheduleNameFreeLocked(id domain.ScheduleID, owner domain.UserID, name string) error {
	for _, o := range s.schedules {
		if o.ID != id && o.OwnerID == owner && strings.EqualFold(o.Name, name) {
			return fmt.Errorf("schedule name %q is taken: %w", name, domain.ErrAlreadyExists)
		}
	}
	return nil
}

// CreateSchedule implements schedules.Repository.
func (s *Store) CreateSchedule(_ context.Context, sc schedules.Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[sc.ID]; ok {
		return fmt.Errorf("schedule %s: %w", sc.ID, domain.ErrAlreadyExists)
	}
	if _, ok := s.users[sc.OwnerID]; !ok {
		return fmt.Errorf("owner %s of schedule %s: %w", sc.OwnerID, sc.ID, domain.ErrNotFound)
	}
	if err := s.checkScheduleLocked(&sc); err != nil {
		return err
	}
	if err := s.scheduleNameFreeLocked(sc.ID, sc.OwnerID, sc.Name); err != nil {
		return err
	}
	s.schedules[sc.ID] = sc
	return nil
}

// GetSchedule implements schedules.Repository.
func (s *Store) GetSchedule(_ context.Context, id domain.ScheduleID) (schedules.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sc, ok := s.schedules[id]
	if !ok {
		return schedules.Schedule{}, fmt.Errorf("schedule %s: %w", id, domain.ErrNotFound)
	}
	return cloneSchedule(sc), nil
}

// GetSchedules implements schedules.Repository.
func (s *Store) GetSchedules(_ context.Context, ids []domain.ScheduleID) ([]schedules.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids = slices.Clone(ids)
	slices.SortFunc(ids, compareIDs)
	var out []schedules.Schedule
	for _, id := range slices.Compact(ids) {
		if sc, ok := s.schedules[id]; ok {
			out = append(out, cloneSchedule(sc))
		}
	}
	return out, nil
}

// ListSchedules implements schedules.Repository.
func (s *Store) ListSchedules(_ context.Context, f schedules.ListFilter) ([]schedules.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []schedules.Schedule
	for _, sc := range s.schedules {
		if (f.OwnerID.IsZero() || sc.OwnerID == f.OwnerID) &&
			(f.RemoteID.IsZero() || slices.Contains(sc.RemoteIDs, f.RemoteID)) &&
			(f.ContactID.IsZero() || slices.ContainsFunc(sc.Contacts, func(l schedules.ContactLink) bool { return l.ContactID == f.ContactID })) {
			out = append(out, cloneSchedule(sc))
		}
	}
	slices.SortFunc(out, func(a, b schedules.Schedule) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
	})
	return out, nil
}

// UpdateSchedule implements schedules.Repository.
func (s *Store) UpdateSchedule(_ context.Context, sc schedules.Schedule, reschedule bool) (schedules.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.schedules[sc.ID]
	if !ok {
		return schedules.Schedule{}, fmt.Errorf("schedule %s: %w", sc.ID, domain.ErrNotFound)
	}
	if cur.Version != sc.Version {
		return schedules.Schedule{}, fmt.Errorf("schedule %s is at version %d, not %d: %w", sc.ID, cur.Version, sc.Version, domain.ErrConflict)
	}
	next := cloneSchedule(sc)
	// Never written: identity, creation and the fire state (unless
	// rescheduled).
	next.OwnerID, next.CreatedAt, next.LastFiredAt = cur.OwnerID, cur.CreatedAt, cur.LastFiredAt
	if !reschedule {
		next.NextRunAt = cur.NextRunAt
	}
	next.Version = cur.Version + 1
	if err := s.checkScheduleLocked(&next); err != nil {
		return schedules.Schedule{}, err
	}
	if err := s.scheduleNameFreeLocked(next.ID, next.OwnerID, next.Name); err != nil {
		return schedules.Schedule{}, err
	}
	if cur.Enabled && !next.Enabled {
		s.cancelOpenRunLocked(next.ID, next.UpdatedAt)
	}
	s.schedules[next.ID] = next
	return cloneSchedule(next), nil
}

// SetScheduleEnabled implements schedules.Repository.
func (s *Store) SetScheduleEnabled(_ context.Context, id domain.ScheduleID, t schedules.Toggle) (schedules.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.schedules[id]
	if !ok {
		return schedules.Schedule{}, fmt.Errorf("schedule %s: %w", id, domain.ErrNotFound)
	}
	sc = cloneSchedule(sc)
	sc.Enabled = t.Enabled
	if t.Enabled {
		sc.NextRunAt, sc.PausedReason = t.NextRunAt, ""
	} else {
		sc.NextRunAt, sc.PausedReason = time.Time{}, t.PausedReason
		s.cancelOpenRunLocked(id, t.At)
	}
	sc.UpdatedAt = t.At
	sc.Version++
	s.schedules[id] = sc
	return cloneSchedule(sc), nil
}

// cancelOpenRunLocked cancels the schedule's open run, if any.
func (s *Store) cancelOpenRunLocked(id domain.ScheduleID, at time.Time) {
	if r, ok := s.openRunLocked(id); ok {
		r.Status, r.Reason, r.Message, r.UpdatedAt = schedules.RunCancelled, schedules.ReasonDisabled, "", at
		s.runs[r.ID] = r
	}
}

// openRunLocked returns the schedule's pending or launching run.
func (s *Store) openRunLocked(id domain.ScheduleID) (schedules.Run, bool) {
	for _, r := range s.runs {
		if r.ScheduleID == id && r.Status.Open() {
			return r, true
		}
	}
	return schedules.Run{}, false
}

// DeleteSchedule implements schedules.Repository.
func (s *Store) DeleteSchedule(_ context.Context, id domain.ScheduleID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[id]; !ok {
		return fmt.Errorf("schedule %s: %w", id, domain.ErrNotFound)
	}
	for rid, r := range s.runs {
		if r.ScheduleID == id {
			delete(s.runs, rid)
			delete(s.runNotified, rid)
		}
	}
	for jid, j := range s.jobs {
		if j.ScheduleID == id {
			j.ScheduleID = domain.ScheduleID{}
			s.jobs[jid] = j
		}
	}
	for did, d := range s.deliveries {
		if d.ScheduleID == id {
			d.ScheduleID = domain.ScheduleID{}
			s.deliveries[did] = d
		}
	}
	delete(s.schedules, id)
	return nil
}

// ListDueSchedules implements schedules.Repository.
func (s *Store) ListDueSchedules(_ context.Context, now time.Time, limit int) ([]schedules.Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []schedules.Schedule
	for _, sc := range s.schedules {
		if sc.Enabled && !sc.NextRunAt.IsZero() && !sc.NextRunAt.After(now) {
			out = append(out, sc)
		}
	}
	slices.SortFunc(out, func(a, b schedules.Schedule) int {
		return cmp.Or(a.NextRunAt.Compare(b.NextRunAt), compareIDs(a.ID, b.ID))
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = cloneSchedule(out[i])
	}
	return out, nil
}

// checkRun enforces the CHECK constraints of a run row.
func checkRun(r schedules.Run) error {
	invalid := func(what string) error {
		return fmt.Errorf("run %s: %w: %s", r.ID, domain.ErrInvalidArgument, what)
	}
	switch {
	case r.Trigger != schedules.TriggerCron && r.Trigger != schedules.TriggerManual:
		return invalid("trigger")
	case !r.Status.Valid():
		return invalid("status")
	case r.Coalesced < 0:
		return invalid("coalesced")
	case (r.Status == schedules.RunLaunching || r.Status == schedules.RunLaunched) && r.JobID.IsZero():
		return invalid("a launching or launched run has no job")
	}
	return nil
}

// insertRunLocked inserts r (or merges it into the open run) for a
// schedule that exists. It changes nothing on error.
func (s *Store) insertRunLocked(r schedules.Run, merge bool) (schedules.Run, error) {
	open, hasOpen := s.openRunLocked(r.ScheduleID)
	if merge && hasOpen {
		open.Coalesced += r.Coalesced + 1
		if r.Deadline.After(open.Deadline) {
			open.Deadline = r.Deadline
		}
		open.UpdatedAt = r.CreatedAt
		s.runs[open.ID] = open
		return open, nil
	}
	if _, ok := s.runs[r.ID]; ok {
		return schedules.Run{}, fmt.Errorf("run %s: %w", r.ID, domain.ErrAlreadyExists)
	}
	if err := checkRun(r); err != nil {
		return schedules.Run{}, err
	}
	if r.Status.Open() && hasOpen {
		return schedules.Run{}, fmt.Errorf("schedule %s has the open run %s: %w", r.ScheduleID, open.ID, domain.ErrAlreadyExists)
	}
	if !r.WorkerID.IsZero() {
		if _, ok := s.workers[r.WorkerID]; !ok {
			return schedules.Run{}, fmt.Errorf("worker %s of run %s: %w", r.WorkerID, r.ID, domain.ErrNotFound)
		}
	}
	r.JobStatus = ""
	s.runs[r.ID] = r
	return r, nil
}

// withJobStatusLocked fills in r.JobStatus from the jobs.
func (s *Store) withJobStatusLocked(r schedules.Run) schedules.Run {
	r.JobStatus = ""
	if j, ok := s.jobs[r.JobID]; ok && !r.JobID.IsZero() {
		r.JobStatus = j.Status
	}
	return r
}

// FireSchedule implements schedules.Repository.
func (s *Store) FireSchedule(_ context.Context, f schedules.Fire) (schedules.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.schedules[f.ID]
	if !ok {
		return schedules.Run{}, fmt.Errorf("schedule %s: %w", f.ID, domain.ErrNotFound)
	}
	if !sc.Enabled || !sc.NextRunAt.Equal(f.ExpectedNext) {
		return schedules.Run{}, fmt.Errorf("fire schedule %s: next run is not %v: %w", f.ID, f.ExpectedNext, domain.ErrConflict)
	}
	r := f.Run
	r.ScheduleID = f.ID
	stored, err := s.insertRunLocked(r, f.Merge)
	if err != nil {
		return schedules.Run{}, err
	}
	sc.NextRunAt, sc.LastFiredAt = f.NewNext, f.Run.CreatedAt
	s.schedules[f.ID] = sc
	return s.withJobStatusLocked(stored), nil
}

// AddRun implements schedules.Repository.
func (s *Store) AddRun(_ context.Context, r schedules.Run, merge bool) (schedules.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[r.ScheduleID]; !ok {
		return schedules.Run{}, fmt.Errorf("schedule %s: %w", r.ScheduleID, domain.ErrNotFound)
	}
	stored, err := s.insertRunLocked(r, merge)
	if err != nil {
		return schedules.Run{}, err
	}
	return s.withJobStatusLocked(stored), nil
}

// GetRun implements schedules.Repository.
func (s *Store) GetRun(_ context.Context, id domain.ScheduleRunID) (schedules.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return schedules.Run{}, fmt.Errorf("run %s: %w", id, domain.ErrNotFound)
	}
	return s.withJobStatusLocked(r), nil
}

// ListOpenRuns implements schedules.Repository.
func (s *Store) ListOpenRuns(_ context.Context, limit int) ([]schedules.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []schedules.Run
	for _, r := range s.runs {
		if r.Status.Open() {
			out = append(out, s.withJobStatusLocked(r))
		}
	}
	slices.SortFunc(out, func(a, b schedules.Run) int {
		return cmp.Or(a.ScheduledFor.Compare(b.ScheduledFor), compareIDs(a.ID, b.ID))
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateRun implements schedules.Repository.
func (s *Store) UpdateRun(_ context.Context, u schedules.RunUpdate) (schedules.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[u.ID]
	if !ok {
		return schedules.Run{}, fmt.Errorf("run %s: %w", u.ID, domain.ErrNotFound)
	}
	if r.Status != u.From {
		return schedules.Run{}, fmt.Errorf("run %s is %s, not %s: %w", u.ID, r.Status, u.From, domain.ErrConflict)
	}
	next := r
	next.Status, next.Reason, next.Message, next.UpdatedAt = u.To, u.Reason, u.Message, u.At
	if !u.WorkerID.IsZero() {
		next.WorkerID = u.WorkerID
	}
	if !u.JobID.IsZero() {
		next.JobID = u.JobID
	}
	if err := checkRun(next); err != nil {
		return schedules.Run{}, err
	}
	if !next.WorkerID.IsZero() {
		if _, ok := s.workers[next.WorkerID]; !ok {
			return schedules.Run{}, fmt.Errorf("worker %s of run %s: %w", next.WorkerID, u.ID, domain.ErrNotFound)
		}
	}
	if next.Status.Open() {
		if open, ok := s.openRunLocked(r.ScheduleID); ok && open.ID != r.ID {
			return schedules.Run{}, fmt.Errorf("schedule %s has the open run %s: %w", r.ScheduleID, open.ID, domain.ErrAlreadyExists)
		}
	}
	s.runs[u.ID] = next
	if u.PauseSchedule {
		if sc, ok := s.schedules[r.ScheduleID]; ok {
			sc.Enabled, sc.NextRunAt, sc.PausedReason, sc.UpdatedAt = false, time.Time{}, u.Reason, u.At
			sc.Version++
			s.schedules[sc.ID] = sc
		}
	}
	return s.withJobStatusLocked(next), nil
}

// matchesRunLocked reports whether r matches every set field of f
// (ignoring Before and Limit when count is set).
func (s *Store) matchesRunLocked(r schedules.Run, f schedules.RunFilter, count bool) bool {
	if !f.OwnerID.IsZero() {
		if sc, ok := s.schedules[r.ScheduleID]; !ok || sc.OwnerID != f.OwnerID {
			return false
		}
	}
	return (f.ScheduleID.IsZero() || r.ScheduleID == f.ScheduleID) &&
		(len(f.Statuses) == 0 || slices.Contains(f.Statuses, r.Status)) &&
		(f.ScheduledFrom.IsZero() || !r.ScheduledFor.Before(f.ScheduledFrom)) &&
		(f.ScheduledUntil.IsZero() || r.ScheduledFor.Before(f.ScheduledUntil)) &&
		(count || f.Before.IsZero() || compareIDs(r.ID, f.Before) < 0)
}

// ListRuns implements schedules.Repository.
func (s *Store) ListRuns(_ context.Context, f schedules.RunFilter) ([]schedules.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []schedules.Run
	for _, r := range s.runs {
		if s.matchesRunLocked(r, f, false) {
			out = append(out, s.withJobStatusLocked(r))
		}
	}
	slices.SortFunc(out, func(a, b schedules.Run) int { return compareIDs(b.ID, a.ID) })
	if f.Limit >= 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// CountRuns implements schedules.Repository.
func (s *Store) CountRuns(_ context.Context, f schedules.RunFilter) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	for _, r := range s.runs {
		if s.matchesRunLocked(r, f, true) {
			n++
		}
	}
	return n, nil
}

// LatestRuns implements schedules.Repository.
func (s *Store) LatestRuns(_ context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[domain.ScheduleID]schedules.Run)
	for _, r := range s.runs {
		if !slices.Contains(ids, r.ScheduleID) {
			continue
		}
		if cur, ok := out[r.ScheduleID]; !ok || compareIDs(r.ID, cur.ID) > 0 {
			out[r.ScheduleID] = s.withJobStatusLocked(r)
		}
	}
	return out, nil
}

// HasActiveRun implements schedules.Repository.
func (s *Store) HasActiveRun(_ context.Context, id domain.ScheduleID) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runs {
		if r.ScheduleID == id && r.Status == schedules.RunLaunching {
			return true, nil
		}
	}
	for _, j := range s.jobs {
		if j.ScheduleID == id && !j.Status.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// ListUpcoming implements schedules.Repository.
func (s *Store) ListUpcoming(_ context.Context, owner domain.UserID, limit int) ([]schedules.Upcoming, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var due []schedules.Schedule
	for _, sc := range s.schedules {
		if sc.Enabled && !sc.NextRunAt.IsZero() && (owner.IsZero() || sc.OwnerID == owner) {
			due = append(due, sc)
		}
	}
	slices.SortFunc(due, func(a, b schedules.Schedule) int {
		return cmp.Or(a.NextRunAt.Compare(b.NextRunAt), compareIDs(a.ID, b.ID))
	})
	if limit >= 0 && len(due) > limit {
		due = due[:limit]
	}
	out := make([]schedules.Upcoming, 0, len(due))
	for _, sc := range due {
		out = append(out, schedules.Upcoming{ScheduleID: sc.ID, Name: sc.Name, At: sc.NextRunAt, Target: sc.Target})
	}
	return out, nil
}
