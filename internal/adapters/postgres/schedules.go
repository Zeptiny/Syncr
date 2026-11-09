package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

var _ schedules.Repository = (*Store)(nil)

// Schedules live in schedules, with their links in schedule_remotes and
// schedule_contacts, and their runs in schedule_runs. A schedule and its
// links change in one transaction (inTx).
//
// The scheduler's races are settled by the statements:
//
//   - FireSchedule's AdvanceScheduleFire is a compare-and-swap on
//     next_run_at: a concurrent fire of the same occurrence waits for the
//     row lock, then re-evaluates the WHERE on the new row and matches
//     nothing (ErrConflict). The run is merged or inserted in the same
//     transaction, so a lost fire leaves no run behind.
//   - The unique partial index schedule_runs_one_pending_key allows one
//     pending run per schedule: an insert (or an update back to pending)
//     that would make a second one fails (ErrAlreadyExists) and rolls its
//     transaction back.
//   - UpdateRun is a compare-and-swap on the run's status, so exactly one
//     of several concurrent pending -> launching updates reserves its job
//     ID.
//   - Lock order: every transaction that writes a schedule's runs locks
//     the schedule row first (UpdateSchedule, SetScheduleEnabled,
//     FireSchedule and DeleteSchedule by writing it, AddRun and UpdateRun
//     with LockScheduleRow and LockRunSchedule), then its runs. Two such
//     transactions therefore cannot deadlock (e.g. an auto-pause racing a
//     user's pause).

// CreateSchedule implements schedules.Repository.
func (s *Store) CreateSchedule(ctx context.Context, sc schedules.Schedule) error {
	op := fmt.Sprintf("create schedule %s", sc.ID)
	return s.inTx(ctx, op, func(q *db.Queries) error {
		if err := q.CreateSchedule(ctx, createScheduleParams(sc)); err != nil {
			return mapErr(op, err)
		}
		return insertScheduleLinks(ctx, q, op, sc)
	})
}

// insertScheduleLinks inserts the remote and contact rows of sc.
func insertScheduleLinks(ctx context.Context, q *db.Queries, op string, sc schedules.Schedule) error {
	for _, id := range sortedUniqueIDs(sc.RemoteIDs) {
		if err := q.InsertScheduleRemote(ctx, db.InsertScheduleRemoteParams{ScheduleID: pgID(sc.ID), RemoteID: pgID(id)}); err != nil {
			return mapErr(fmt.Sprintf("%s: remote %s", op, id), err)
		}
	}
	for _, l := range sc.Contacts {
		events := make([]string, len(l.Events))
		for i, e := range l.Events {
			events[i] = string(e)
		}
		if err := q.InsertScheduleContact(ctx, db.InsertScheduleContactParams{
			ScheduleID: pgID(sc.ID), ContactID: pgID(l.ContactID), Events: events,
		}); err != nil {
			return mapErr(fmt.Sprintf("%s: contact %s", op, l.ContactID), err)
		}
	}
	return nil
}

// GetSchedule implements schedules.Repository.
func (s *Store) GetSchedule(ctx context.Context, id domain.ScheduleID) (schedules.Schedule, error) {
	op := fmt.Sprintf("get schedule %s", id)
	row, err := s.q.GetSchedule(ctx, pgID(id))
	if err != nil {
		return schedules.Schedule{}, mapErr(op, err)
	}
	out, err := withScheduleLinks(ctx, s.q, op, []db.Schedule{row})
	if err != nil {
		return schedules.Schedule{}, err
	}
	return out[0], nil
}

// GetSchedules implements schedules.Repository.
func (s *Store) GetSchedules(ctx context.Context, ids []domain.ScheduleID) ([]schedules.Schedule, error) {
	const op = "get schedules"
	if len(ids) == 0 {
		return []schedules.Schedule{}, nil
	}
	rows, err := s.q.GetSchedules(ctx, pgIDs(ids))
	if err != nil {
		return nil, mapErr(op, err)
	}
	return withScheduleLinks(ctx, s.q, op, rows)
}

// ListSchedules implements schedules.Repository.
func (s *Store) ListSchedules(ctx context.Context, f schedules.ListFilter) ([]schedules.Schedule, error) {
	const op = "list schedules"
	rows, err := s.q.ListSchedules(ctx, db.ListSchedulesParams{
		OwnerID: pgOptID(f.OwnerID), RemoteID: pgOptID(f.RemoteID), ContactID: pgOptID(f.ContactID),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	return withScheduleLinks(ctx, s.q, op, rows)
}

// UpdateSchedule implements schedules.Repository: the row is locked first
// (LockSchedule), which decides the version check (ErrNotFound,
// ErrConflict) and whether Enabled changes to false; then the row is
// updated, its links replaced and, when it was disabled, its open run
// cancelled, in one transaction.
func (s *Store) UpdateSchedule(ctx context.Context, sc schedules.Schedule, reschedule bool) (schedules.Schedule, error) {
	op := fmt.Sprintf("update schedule %s", sc.ID)
	var out schedules.Schedule
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		cur, err := q.LockSchedule(ctx, pgID(sc.ID))
		if err != nil {
			return mapErr(op, err) // no row: ErrNotFound
		}
		if cur.Version != sc.Version {
			return fmt.Errorf("%s: at version %d, not %d: %w", op, cur.Version, sc.Version, domain.ErrConflict)
		}
		row, err := q.UpdateSchedule(ctx, db.UpdateScheduleParams{
			Name: sc.Name, Description: sc.Description, Cron: sc.Cron, Timezone: sc.Timezone,
			Kind: sc.Kind, Definition: sc.Definition, SearchText: cleanText(sc.SearchText),
			WorkerID: pgOptID(sc.Target.WorkerID), WorkerLabel: optText(sc.Target.Label),
			MissedAction: string(sc.Missed.Action), MaxDelayNs: int64(sc.Missed.MaxDelay), Overlap: string(sc.Overlap),
			Enabled: sc.Enabled, PausedReason: sc.PausedReason,
			Reschedule: reschedule, NextRunAt: pgOptTime(sc.NextRunAt), UpdatedAt: pgTime(sc.UpdatedAt),
			ID: pgID(sc.ID), ExpectedVersion: sc.Version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", op, domain.ErrConflict) // cannot happen under the lock
		}
		if err != nil {
			return mapErr(op, err)
		}
		if err := q.DeleteScheduleRemotes(ctx, pgID(sc.ID)); err != nil {
			return mapErr(op, err)
		}
		if err := q.DeleteScheduleContacts(ctx, pgID(sc.ID)); err != nil {
			return mapErr(op, err)
		}
		if err := insertScheduleLinks(ctx, q, op, sc); err != nil {
			return err
		}
		if cur.Enabled && !row.Enabled {
			if err := q.CancelOpenRun(ctx, db.CancelOpenRunParams{UpdatedAt: pgTime(sc.UpdatedAt), ScheduleID: pgID(sc.ID)}); err != nil {
				return mapErr(op, err)
			}
		}
		out, err = scheduleFrom(row, sortedUniqueIDs(sc.RemoteIDs), cloneContactLinks(sc.Contacts))
		return err
	})
	if err != nil {
		return schedules.Schedule{}, err
	}
	return out, nil
}

// SetScheduleEnabled implements schedules.Repository.
func (s *Store) SetScheduleEnabled(ctx context.Context, id domain.ScheduleID, t schedules.Toggle) (schedules.Schedule, error) {
	op := fmt.Sprintf("set schedule %s enabled", id)
	var out schedules.Schedule
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		p := db.SetScheduleEnabledParams{Enabled: t.Enabled, UpdatedAt: pgTime(t.At), ID: pgID(id)}
		if t.Enabled {
			p.NextRunAt = pgOptTime(t.NextRunAt)
		} else {
			p.PausedReason = t.PausedReason
		}
		row, err := q.SetScheduleEnabled(ctx, p)
		if err != nil {
			return mapErr(op, err) // no row: ErrNotFound
		}
		if !t.Enabled {
			if err := q.CancelOpenRun(ctx, db.CancelOpenRunParams{UpdatedAt: pgTime(t.At), ScheduleID: pgID(id)}); err != nil {
				return mapErr(op, err)
			}
		}
		scheds, err := withScheduleLinks(ctx, q, op, []db.Schedule{row})
		if err != nil {
			return err
		}
		out = scheds[0]
		return nil
	})
	if err != nil {
		return schedules.Schedule{}, err
	}
	return out, nil
}

// DeleteSchedule implements schedules.Repository.
func (s *Store) DeleteSchedule(ctx context.Context, id domain.ScheduleID) error {
	op := fmt.Sprintf("delete schedule %s", id)
	n, err := s.q.DeleteSchedule(ctx, pgID(id))
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// ListDueSchedules implements schedules.Repository.
func (s *Store) ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]schedules.Schedule, error) {
	const op = "list due schedules"
	rows, err := s.q.ListDueSchedules(ctx, db.ListDueSchedulesParams{Now: pgTime(now), RowLimit: limitArg(limit)})
	if err != nil {
		return nil, mapErr(op, err)
	}
	return withScheduleLinks(ctx, s.q, op, rows)
}

// FireSchedule implements schedules.Repository: the compare-and-swap of
// the fire state, then the merge or insert of the run, in one transaction.
func (s *Store) FireSchedule(ctx context.Context, f schedules.Fire) (schedules.Run, error) {
	op := fmt.Sprintf("fire schedule %s", f.ID)
	var out schedules.Run
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		n, err := q.AdvanceScheduleFire(ctx, db.AdvanceScheduleFireParams{
			NewNext: pgOptTime(f.NewNext), FiredAt: pgTime(f.Run.CreatedAt),
			ID: pgID(f.ID), ExpectedNext: pgOptTime(f.ExpectedNext),
		})
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			exists, err := q.ScheduleExists(ctx, pgID(f.ID))
			switch {
			case err != nil:
				return mapErr(op, err)
			case !exists:
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			}
			return fmt.Errorf("%s: disabled, or its next run is not %v: %w", op, f.ExpectedNext, domain.ErrConflict)
		}
		r := f.Run
		r.ScheduleID = f.ID
		id, err := addRun(ctx, q, op, r, f.Merge, &f)
		if err != nil {
			return err
		}
		out, err = getRun(ctx, q, op, id)
		return err
	})
	if err != nil {
		return schedules.Run{}, err
	}
	return out, nil
}

// AddRun implements schedules.Repository.
func (s *Store) AddRun(ctx context.Context, r schedules.Run, merge bool) (schedules.Run, error) {
	op := fmt.Sprintf("add run %s", r.ID)
	var out schedules.Run
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		if _, err := q.LockScheduleRow(ctx, pgID(r.ScheduleID)); err != nil {
			return mapErr(op, err) // no row: ErrNotFound
		}
		id, err := addRun(ctx, q, op, r, merge, nil)
		if err != nil {
			return err
		}
		out, err = getRun(ctx, q, op, id)
		return err
	})
	if err != nil {
		return schedules.Run{}, err
	}
	return out, nil
}

// addRun merges r into its schedule's pending run (if merge and there is
// one) or inserts it; it returns the ID of the run that holds it. For a
// fire (f set) a pending run whose deadline is not after the fire time is
// first superseded (ended missed), f.Earlier is inserted, and a remaining
// pending run absorbs r keeping its deadline; a manual request (f nil)
// extends the pending run's deadline to its own. A missing schedule fails
// the insert's foreign key (ErrNotFound).
func addRun(ctx context.Context, q *db.Queries, op string, r schedules.Run, merge bool, f *schedules.Fire) (pgtype.UUID, error) {
	if f != nil {
		if err := q.SupersedePendingRun(ctx, db.SupersedePendingRunParams{
			Reason: f.MissedReason, Message: cleanText(f.MissedMessage),
			UpdatedAt: pgTime(r.CreatedAt), ScheduleID: pgID(r.ScheduleID),
		}); err != nil {
			return pgtype.UUID{}, mapErr(op, err)
		}
		if f.Earlier != nil {
			e := *f.Earlier
			e.ScheduleID = r.ScheduleID
			if err := insertRun(ctx, q, op, e); err != nil {
				return pgtype.UUID{}, err
			}
		}
	}
	if merge {
		p := db.MergePendingRunParams{
			AddCoalesced: int32(r.Coalesced), //nolint:gosec // G115: at most schedules.MaxCoalesce per fire
			UpdatedAt:    pgTime(r.CreatedAt), ScheduleID: pgID(r.ScheduleID),
		}
		if f != nil {
			p.OpenAfter = pgTime(r.CreatedAt)
		} else {
			p.ExtendTo = pgTime(r.Deadline)
		}
		id, err := q.MergePendingRun(ctx, p)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, mapErr(op, err)
		}
	}
	if err := insertRun(ctx, q, op, r); err != nil {
		return pgtype.UUID{}, err
	}
	return pgID(r.ID), nil
}

// insertRun inserts r.
func insertRun(ctx context.Context, q *db.Queries, op string, r schedules.Run) error {
	if err := q.InsertRun(ctx, db.InsertRunParams{
		ID: pgID(r.ID), ScheduleID: pgID(r.ScheduleID), Trigger: r.Trigger,
		ScheduledFor: pgTime(r.ScheduledFor), Deadline: pgTime(r.Deadline),
		Coalesced: int32(r.Coalesced), //nolint:gosec // G115: at most schedules.MaxCoalesce per fire
		Status:    string(r.Status), Reason: r.Reason, Message: cleanText(r.Message),
		WorkerID: pgOptID(r.WorkerID), JobID: pgOptID(r.JobID),
		CreatedAt: pgTime(r.CreatedAt), UpdatedAt: pgTime(r.UpdatedAt),
	}); err != nil {
		return mapErr(op, err)
	}
	return nil
}

// GetRun implements schedules.Repository.
func (s *Store) GetRun(ctx context.Context, id domain.ScheduleRunID) (schedules.Run, error) {
	op := fmt.Sprintf("get run %s", id)
	return getRun(ctx, s.q, op, pgID(id))
}

func getRun(ctx context.Context, q *db.Queries, op string, id pgtype.UUID) (schedules.Run, error) {
	row, err := q.GetRun(ctx, id)
	if err != nil {
		return schedules.Run{}, mapErr(op, err)
	}
	r, err := runFrom(row.ScheduleRun, row.JobStatus)
	if err != nil {
		return schedules.Run{}, fmt.Errorf("%s: %w", op, err)
	}
	return r, nil
}

// ListOpenRuns implements schedules.Repository.
func (s *Store) ListOpenRuns(ctx context.Context, limit int) ([]schedules.Run, error) {
	const op = "list open runs"
	rows, err := s.q.ListOpenRuns(ctx, limitArg(limit))
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]schedules.Run, 0, len(rows))
	for _, row := range rows {
		r, err := runFrom(row.ScheduleRun, row.JobStatus)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// UpdateRun implements schedules.Repository: the compare-and-swap of the
// status and, with PauseSchedule, the auto-pause, in one transaction. The
// run's schedule is locked first (the lock order above). When the update
// matches nothing, a second query tells a missing run (ErrNotFound) from a
// status mismatch (ErrConflict).
func (s *Store) UpdateRun(ctx context.Context, u schedules.RunUpdate) (schedules.Run, error) {
	op := fmt.Sprintf("update run %s", u.ID)
	var out schedules.Run
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		if _, err := q.LockRunSchedule(ctx, pgID(u.ID)); err != nil {
			return mapErr(op, err) // no row: the run does not exist (ErrNotFound)
		}
		schedID, err := q.UpdateRunStatus(ctx, db.UpdateRunStatusParams{
			ToStatus: string(u.To), Reason: u.Reason, Message: cleanText(u.Message),
			WorkerID: pgOptID(u.WorkerID), JobID: pgOptID(u.JobID), UpdatedAt: pgTime(u.At),
			ID: pgID(u.ID), FromStatus: string(u.From),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			exists, qerr := q.RunExists(ctx, pgID(u.ID))
			switch {
			case qerr != nil:
				return mapErr(op, qerr)
			case !exists:
				return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
			}
			return fmt.Errorf("%s: not %s, or being cancelled: %w", op, u.From, domain.ErrConflict)
		}
		if err != nil {
			return mapErr(op, err)
		}
		if u.PauseSchedule {
			if err := q.PauseSchedule(ctx, db.PauseScheduleParams{PausedReason: u.Reason, UpdatedAt: pgTime(u.At), ID: schedID}); err != nil {
				return mapErr(op, err)
			}
		}
		out, err = getRun(ctx, q, op, pgID(u.ID))
		return err
	})
	if err != nil {
		return schedules.Run{}, err
	}
	return out, nil
}

// ListRuns implements schedules.Repository.
func (s *Store) ListRuns(ctx context.Context, f schedules.RunFilter) ([]schedules.Run, error) {
	const op = "list runs"
	rows, err := s.q.ListRuns(ctx, db.ListRunsParams{
		ScheduleID: pgOptID(f.ScheduleID), OwnerID: pgOptID(f.OwnerID), Statuses: runStatuses(f.Statuses),
		ScheduledFrom: pgOptTime(f.ScheduledFrom), ScheduledUntil: pgOptTime(f.ScheduledUntil),
		Before: pgOptID(f.Before), RowLimit: limitArg(f.Limit),
	})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]schedules.Run, 0, len(rows))
	for _, row := range rows {
		r, err := runFrom(row.ScheduleRun, row.JobStatus)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// CountRuns implements schedules.Repository.
func (s *Store) CountRuns(ctx context.Context, f schedules.RunFilter) (int64, error) {
	n, err := s.q.CountRuns(ctx, db.CountRunsParams{
		ScheduleID: pgOptID(f.ScheduleID), OwnerID: pgOptID(f.OwnerID), Statuses: runStatuses(f.Statuses),
		ScheduledFrom: pgOptTime(f.ScheduledFrom), ScheduledUntil: pgOptTime(f.ScheduledUntil),
	})
	if err != nil {
		return 0, mapErr("count runs", err)
	}
	return n, nil
}

// LatestRuns implements schedules.Repository.
func (s *Store) LatestRuns(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Run, error) {
	const op = "latest runs"
	out := make(map[domain.ScheduleID]schedules.Run)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.LatestRuns(ctx, pgIDs(ids))
	if err != nil {
		return nil, mapErr(op, err)
	}
	for _, row := range rows {
		r, err := runFrom(row.ScheduleRun, row.JobStatus)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out[r.ScheduleID] = r
	}
	return out, nil
}

// HasActiveRun implements schedules.Repository.
func (s *Store) HasActiveRun(ctx context.Context, id domain.ScheduleID) (bool, error) {
	active, err := s.q.HasActiveRun(ctx, pgID(id))
	if err != nil {
		return false, mapErr(fmt.Sprintf("active run of schedule %s", id), err)
	}
	return active, nil
}

// ListUpcoming implements schedules.Repository.
func (s *Store) ListUpcoming(ctx context.Context, owner domain.UserID, limit int) ([]schedules.Upcoming, error) {
	const op = "list upcoming runs"
	rows, err := s.q.ListUpcoming(ctx, db.ListUpcomingParams{OwnerID: pgOptID(owner), RowLimit: limitArg(limit)})
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]schedules.Upcoming, 0, len(rows))
	for _, row := range rows {
		u := schedules.Upcoming{Name: row.Name, At: timeFrom(row.NextRunAt)}
		if row.WorkerLabel != nil {
			u.Target.Label = *row.WorkerLabel
		}
		if err := decodeID(&u.ScheduleID, row.ID); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if err := decodeID(&u.Target.WorkerID, row.WorkerID); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, u)
	}
	return out, nil
}

// Conversions.

func createScheduleParams(sc schedules.Schedule) db.CreateScheduleParams {
	return db.CreateScheduleParams{
		ID: pgID(sc.ID), OwnerID: pgID(sc.OwnerID), Name: sc.Name, Description: sc.Description,
		Cron: sc.Cron, Timezone: sc.Timezone, Kind: sc.Kind, Definition: sc.Definition, SearchText: cleanText(sc.SearchText),
		WorkerID: pgOptID(sc.Target.WorkerID), WorkerLabel: optText(sc.Target.Label),
		MissedAction: string(sc.Missed.Action), MaxDelayNs: int64(sc.Missed.MaxDelay), Overlap: string(sc.Overlap),
		Enabled: sc.Enabled, PausedReason: sc.PausedReason,
		NextRunAt: pgOptTime(sc.NextRunAt), LastFiredAt: pgOptTime(sc.LastFiredAt),
		CreatedAt: pgTime(sc.CreatedAt), UpdatedAt: pgTime(sc.UpdatedAt), Version: sc.Version,
	}
}

// withScheduleLinks converts rows, loading the links of all of them with
// two queries.
func withScheduleLinks(ctx context.Context, q *db.Queries, op string, rows []db.Schedule) ([]schedules.Schedule, error) {
	out := make([]schedules.Schedule, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]pgtype.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	remoteRows, err := q.ListScheduleRemotes(ctx, ids)
	if err != nil {
		return nil, mapErr(op, err)
	}
	contactRows, err := q.ListScheduleContacts(ctx, ids)
	if err != nil {
		return nil, mapErr(op, err)
	}
	remoteIDs := map[pgtype.UUID][]domain.RemoteID{}
	for _, r := range remoteRows {
		var id domain.RemoteID
		if err := decodeID(&id, r.RemoteID); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		remoteIDs[r.ScheduleID] = append(remoteIDs[r.ScheduleID], id)
	}
	links := map[pgtype.UUID][]schedules.ContactLink{}
	for _, c := range contactRows {
		l := schedules.ContactLink{Events: make(domain.EventSet, len(c.Events))}
		if err := decodeID(&l.ContactID, c.ContactID); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		for i, e := range c.Events {
			l.Events[i] = domain.NotificationEvent(e)
		}
		links[c.ScheduleID] = append(links[c.ScheduleID], l)
	}
	for _, row := range rows {
		sc, err := scheduleFrom(row, remoteIDs[row.ID], links[row.ID])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, sc)
	}
	return out, nil
}

// scheduleFrom converts a row and its links (sorted by ID).
func scheduleFrom(row db.Schedule, remoteIDs []domain.RemoteID, contacts []schedules.ContactLink) (schedules.Schedule, error) {
	sc := schedules.Schedule{
		Name: row.Name, Description: row.Description, Cron: row.Cron, Timezone: row.Timezone,
		Kind: row.Kind, Definition: row.Definition, RemoteIDs: remoteIDs, SearchText: row.SearchText,
		Missed:  schedules.MissedPolicy{Action: schedules.MissedAction(row.MissedAction), MaxDelay: time.Duration(row.MaxDelayNs)},
		Overlap: schedules.Overlap(row.Overlap), Enabled: row.Enabled, PausedReason: row.PausedReason,
		Contacts:  contacts,
		NextRunAt: timeFrom(row.NextRunAt), LastFiredAt: timeFrom(row.LastFiredAt),
		CreatedAt: timeFrom(row.CreatedAt), UpdatedAt: timeFrom(row.UpdatedAt), Version: row.Version,
	}
	if row.WorkerLabel != nil {
		sc.Target.Label = *row.WorkerLabel
	}
	for _, d := range []struct {
		dst interface{ UnmarshalText([]byte) error }
		src pgtype.UUID
	}{{&sc.ID, row.ID}, {&sc.OwnerID, row.OwnerID}, {&sc.Target.WorkerID, row.WorkerID}} {
		if err := decodeID(d.dst, d.src); err != nil {
			return schedules.Schedule{}, err
		}
	}
	return sc, nil
}

// runFrom converts a run row and its job's status (NULL: no job).
func runFrom(row db.ScheduleRun, jobStatus *string) (schedules.Run, error) {
	r := schedules.Run{
		Trigger: row.Trigger, ScheduledFor: timeFrom(row.ScheduledFor), Deadline: timeFrom(row.Deadline),
		Coalesced: int(row.Coalesced), Status: schedules.RunStatus(row.Status), Reason: row.Reason, Message: row.Message,
		CreatedAt: timeFrom(row.CreatedAt), UpdatedAt: timeFrom(row.UpdatedAt),
	}
	if jobStatus != nil {
		r.JobStatus = domain.JobStatus(*jobStatus)
	}
	for _, d := range []struct {
		dst interface{ UnmarshalText([]byte) error }
		src pgtype.UUID
	}{{&r.ID, row.ID}, {&r.ScheduleID, row.ScheduleID}, {&r.WorkerID, row.WorkerID}, {&r.JobID, row.JobID}} {
		if err := decodeID(d.dst, d.src); err != nil {
			return schedules.Run{}, err
		}
	}
	return r, nil
}

func runStatuses(ss []schedules.RunStatus) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// optText maps "" to NULL.
func optText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pgIDs[K any](ids []domain.ID[K]) []pgtype.UUID {
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		out[i] = pgID(id)
	}
	return out
}

// sortedUniqueIDs returns ids sorted as Postgres sorts uuids (by bytes),
// without duplicates; nil for none.
func sortedUniqueIDs[K any](ids []domain.ID[K]) []domain.ID[K] {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b domain.ID[K]) int {
		ua, ub := a.UUID(), b.UUID()
		return bytes.Compare(ua[:], ub[:])
	})
	return slices.Compact(out)
}

func cloneContactLinks(ls []schedules.ContactLink) []schedules.ContactLink {
	if len(ls) == 0 {
		return nil
	}
	out := slices.Clone(ls)
	for i := range out {
		out[i].Events = slices.Clone(out[i].Events)
	}
	return out
}
