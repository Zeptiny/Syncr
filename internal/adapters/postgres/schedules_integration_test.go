//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// These tests cover what the contract suite cannot: races across two
// connection pools (two controllers on one database), the schedule service
// firing over Postgres, and contact links (inserted with plain SQL while
// the contacts adapter is a stub).

var schedT0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// twoStores returns two stores (pools) on one fresh migrated database.
func twoStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	name := createDB(t, true)
	return openStore(t, name), openStore(t, name)
}

// schedTestFixture inserts a user and a worker and returns a valid
// schedule of the user on the worker, not stored yet.
func schedTestFixture(t *testing.T, s *Store, name string) schedules.Schedule {
	t.Helper()
	return schedules.Schedule{
		ID: domain.NewScheduleID(), OwnerID: insertJobsTestUser(t, s), Name: name,
		Cron: "0 * * * *", Timezone: "UTC", Kind: "rclone_transfer",
		Definition: []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY"}}`),
		Target:     schedules.Target{WorkerID: schedTestWorker(t, s)},
		Missed:     schedules.MissedPolicy{Action: schedules.MissedRunLate},
		Overlap:    schedules.OverlapSkip, Enabled: true, NextRunAt: schedT0.Add(time.Hour),
		CreatedAt: schedT0, UpdatedAt: schedT0, Version: 1,
	}
}

// schedTestWorker inserts a worker with a unique name.
func schedTestWorker(t *testing.T, s *Store) domain.WorkerID {
	t.Helper()
	w := workers.Worker{ID: domain.NewWorkerID(), Mode: domain.WorkerActive, CreatedAt: testTime()}
	w.Name = "w-" + w.ID.String()
	if err := s.CreateWorker(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func pendingRun(sc schedules.Schedule, at time.Time) schedules.Run {
	return schedules.Run{
		ID: domain.NewScheduleRunID(), ScheduleID: sc.ID, Trigger: schedules.TriggerCron,
		ScheduledFor: at, Deadline: at.Add(time.Hour), Status: schedules.RunPending, CreatedAt: at, UpdatedAt: at,
	}
}

// TestFireScheduleRaceAcrossPools: concurrent fires of one occurrence from
// two pools store one fire and one run.
func TestFireScheduleRaceAcrossPools(t *testing.T) {
	t.Parallel()
	a, b := twoStores(t)
	ctx := context.Background()
	sc := schedTestFixture(t, a, "race")
	if err := a.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		cur, err := a.GetSchedule(ctx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := range 24 {
			s := a
			if i%2 == 1 {
				s = b
			}
			wg.Go(func() {
				r := pendingRun(sc, cur.NextRunAt)
				r.Deadline = r.ScheduledFor.Add(3 * time.Hour) // the later fires merge into the first run
				_, err := s.FireSchedule(ctx, schedules.Fire{
					ID: sc.ID, ExpectedNext: cur.NextRunAt, NewNext: cur.NextRunAt.Add(time.Hour),
					Run: r, Merge: true,
				})
				switch {
				case err == nil:
					mu.Lock()
					wins++
					mu.Unlock()
				case !errors.Is(err, domain.ErrConflict):
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d fires won, want 1", round, wins)
		}
	}
	// Three fires: one open run that absorbed the two later ones.
	rs, err := b.ListRuns(ctx, schedules.RunFilter{ScheduleID: sc.ID, Limit: -1})
	if err != nil || len(rs) != 1 || rs[0].Coalesced != 2 {
		t.Errorf("runs = (%+v, %v), want one with two merged", rs, err)
	}
	if got, _ := b.GetSchedule(ctx, sc.ID); !got.NextRunAt.Equal(sc.NextRunAt.Add(3*time.Hour)) || got.Version != 1 {
		t.Errorf("fire state = next %v, v%d", got.NextRunAt, got.Version)
	}
}

// TestOneOpenRunAcrossPools: concurrent inserts of open runs leave one; the
// others fail with ErrAlreadyExists and change nothing.
func TestOneOpenRunAcrossPools(t *testing.T) {
	t.Parallel()
	a, b := twoStores(t)
	ctx := context.Background()
	sc := schedTestFixture(t, a, "open")
	if err := a.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := range 16 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			_, err := s.AddRun(ctx, pendingRun(sc, schedT0), false)
			switch {
			case err == nil:
				mu.Lock()
				inserted++
				mu.Unlock()
			case !errors.Is(err, domain.ErrAlreadyExists):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	open, err := a.ListOpenRuns(ctx, -1)
	if inserted != 1 || err != nil || len(open) != 1 {
		t.Errorf("inserted %d, open runs (%d, %v); want one", inserted, len(open), err)
	}
	// Merging requests from both pools all land in that run.
	for i := range 10 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			for {
				_, err := s.AddRun(ctx, pendingRun(sc, schedT0), true)
				if !errors.Is(err, domain.ErrAlreadyExists) {
					if err != nil {
						t.Error(err)
					}
					return
				}
			}
		})
	}
	wg.Wait()
	if r, err := a.GetRun(ctx, open[0].ID); err != nil || r.Coalesced != 10 {
		t.Errorf("open run after the merges = (%+v, %v)", r, err)
	}
}

// TestLaunchClaimAcrossPools: of concurrent pending -> launching updates
// from two pools exactly one reserves its job ID; ending and pausing is
// one transaction.
func TestLaunchClaimAcrossPools(t *testing.T) {
	t.Parallel()
	a, b := twoStores(t)
	ctx := context.Background()
	sc := schedTestFixture(t, a, "claim")
	if err := a.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	r, err := a.AddRun(ctx, pendingRun(sc, schedT0), false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []domain.JobID
	for i := range 20 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			job := domain.NewJobID()
			got, err := s.UpdateRun(ctx, schedules.RunUpdate{
				ID: r.ID, From: schedules.RunPending, To: schedules.RunLaunching,
				WorkerID: sc.Target.WorkerID, JobID: job, At: schedT0,
			})
			switch {
			case err == nil:
				if got.JobID != job {
					t.Errorf("the winner's run has job %s, not %s", got.JobID, job)
				}
				mu.Lock()
				winners = append(winners, job)
				mu.Unlock()
			case !errors.Is(err, domain.ErrConflict):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d claims won, want 1", len(winners))
	}
	if got, _ := b.GetRun(ctx, r.ID); got.JobID != winners[0] || got.Status != schedules.RunLaunching {
		t.Errorf("claimed run = %+v", got)
	}
	if active, err := b.HasActiveRun(ctx, sc.ID); err != nil || !active {
		t.Errorf("HasActiveRun(launching) = (%v, %v)", active, err)
	}
	// Ending the run and pausing its schedule is one step.
	if _, err := a.UpdateRun(ctx, schedules.RunUpdate{
		ID: r.ID, From: schedules.RunLaunching, To: schedules.RunFailed, Reason: schedules.ReasonWorkerRemoved,
		Message: "removed \x00 worker", At: schedT0.Add(time.Minute), PauseSchedule: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := b.GetSchedule(ctx, sc.ID)
	run, _ := b.GetRun(ctx, r.ID)
	if got.Enabled || !got.NextRunAt.IsZero() || got.PausedReason != schedules.ReasonWorkerRemoved || got.Version != 2 ||
		run.Status != schedules.RunFailed || run.Message != "removed � worker" {
		t.Errorf("after the auto-pause: schedule %+v, run %+v", got, run)
	}
}

// TestEditDoesNotRewindFire: an edit based on a read from before a fire
// keeps the fire's next run (fires do not bump the version).
func TestEditDoesNotRewindFire(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := context.Background()
	sc := schedTestFixture(t, s, "edit")
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	read, _ := s.GetSchedule(ctx, sc.ID)
	if _, err := s.FireSchedule(ctx, schedules.Fire{
		ID: sc.ID, ExpectedNext: read.NextRunAt, NewNext: read.NextRunAt.Add(time.Hour), Run: pendingRun(sc, read.NextRunAt), Merge: true,
	}); err != nil {
		t.Fatal(err)
	}
	read.Name = "edited"
	got, err := s.UpdateSchedule(ctx, read, false)
	if err != nil || got.Name != "edited" || !got.NextRunAt.Equal(sc.NextRunAt.Add(time.Hour)) || !got.LastFiredAt.Equal(sc.NextRunAt) {
		t.Errorf("UpdateSchedule after a fire = (%+v, %v)", got, err)
	}
}

// insertTestContact inserts a contact row of owner.
func insertTestContact(t *testing.T, s *Store, owner domain.UserID, name string) domain.ContactID {
	t.Helper()
	id := domain.NewContactID()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO contacts (id, owner_id, name, channel_type, created_at, updated_at)
		VALUES ($1, $2, $3, 'webhook', now(), now())`, pgID(id), pgID(owner), name); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestScheduleContactLinks(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := context.Background()
	sc := schedTestFixture(t, s, "linked")
	c1, c2 := insertTestContact(t, s, sc.OwnerID, "ops"), insertTestContact(t, s, sc.OwnerID, "mail")
	links := []schedules.ContactLink{
		{ContactID: c1, Events: domain.EventSet{domain.EventFailed, domain.EventMissed}},
		{ContactID: c2, Events: domain.EventSet{domain.EventSucceeded}},
	}
	slices.SortFunc(links, func(a, b schedules.ContactLink) int {
		ua, ub := a.ContactID.UUID(), b.ContactID.UUID()
		return slices.Compare(ua[:], ub[:])
	})
	sc.Contacts = links
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSchedule(ctx, sc.ID)
	if err != nil || len(got.Contacts) != 2 || got.Contacts[0].ContactID != links[0].ContactID ||
		!slices.Equal(got.Contacts[0].Events, links[0].Events) || !slices.Equal(got.Contacts[1].Events, links[1].Events) {
		t.Fatalf("contacts = (%+v, %v)", got.Contacts, err)
	}
	byContact, err := s.ListSchedules(ctx, schedules.ListFilter{ContactID: c2})
	if err != nil || len(byContact) != 1 || byContact[0].ID != sc.ID {
		t.Errorf("ListSchedules(contact) = (%v, %v)", byContact, err)
	}

	twice := schedTestFixture(t, s, "twice")
	twice.OwnerID = sc.OwnerID
	twice.Contacts = []schedules.ContactLink{links[0], links[0]}
	if err := s.CreateSchedule(ctx, twice); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("CreateSchedule(contact twice) = %v", err)
	}
	bad := schedTestFixture(t, s, "bad events")
	bad.OwnerID = sc.OwnerID
	bad.Contacts = []schedules.ContactLink{{ContactID: c1, Events: domain.EventSet{domain.EventTest}}}
	if err := s.CreateSchedule(ctx, bad); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("CreateSchedule(test event) = %v", err)
	}
	if _, err := s.GetSchedule(ctx, twice.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a refused schedule was stored: %v", err)
	}

	// Replacing the links on update; deleting a contact drops its link.
	got.Contacts = got.Contacts[1:]
	if got, err = s.UpdateSchedule(ctx, got, false); err != nil || len(got.Contacts) != 1 {
		t.Fatalf("UpdateSchedule(links) = (%+v, %v)", got.Contacts, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, pgID(got.Contacts[0].ContactID)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSchedule(ctx, sc.ID); err != nil || len(got.Contacts) != 0 {
		t.Errorf("contacts after deleting the contact = (%+v, %v)", got.Contacts, err)
	}
}

// schedTestClock and schedTestIDs drive the schedules service in TestServiceFireDue.
type schedTestClock struct{ t time.Time }

func (c schedTestClock) Now() time.Time { return c.t }

type schedTestIDs struct{}

func (schedTestIDs) NewScheduleID() domain.ScheduleID       { return domain.NewScheduleID() }
func (schedTestIDs) NewScheduleRunID() domain.ScheduleRunID { return domain.NewScheduleRunID() }

type schedTestRemotes struct{}

func (schedTestRemotes) Usable(context.Context, domain.UserID, []domain.RemoteID) (map[domain.RemoteID]string, error) {
	return map[domain.RemoteID]string{}, nil
}

type schedTestContacts struct{}

func (schedTestContacts) Usable(context.Context, domain.UserID, []domain.ContactID) (map[domain.ContactID]string, error) {
	return map[domain.ContactID]string{}, nil
}

type schedTestWorkers struct{}

func (schedTestWorkers) CheckWorker(context.Context, domain.WorkerID) error { return nil }

type schedTestWaker struct{}

func (schedTestWaker) Wake() {}

// TestServiceFireDue runs the schedules service's FireDue from two
// controllers (pools) at once over Postgres, after downtime: each due
// schedule fires once, with the missed occurrences coalesced.
func TestServiceFireDue(t *testing.T) {
	t.Parallel()
	a, b := twoStores(t)
	ctx := context.Background()
	now := schedT0.Add(5*time.Hour + 30*time.Minute) // 17:30: 13:00..17:00 are due
	newSvc := func(s *Store) *schedules.Service {
		svc, err := schedules.New(schedules.Deps{
			Repo: s, Remotes: schedTestRemotes{}, Contacts: schedTestContacts{}, Workers: schedTestWorkers{},
			Waker: schedTestWaker{}, Clock: schedTestClock{now}, IDs: schedTestIDs{},
		}, schedules.Config{FireBatch: 3})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	svcA, svcB := newSvc(a), newSvc(b)
	var ids []domain.ScheduleID
	for i := range 7 {
		sc := schedTestFixture(t, a, "due-"+string(rune('a'+i)))
		if err := a.CreateSchedule(ctx, sc); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sc.ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	fired := 0
	for i := range 6 {
		svc := svcA
		if i%2 == 1 {
			svc = svcB
		}
		wg.Go(func() {
			n, err := svc.FireDue(ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			fired += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if fired != len(ids) {
		t.Errorf("fired %d, want %d", fired, len(ids))
	}
	for _, id := range ids {
		rs, err := a.ListRuns(ctx, schedules.RunFilter{ScheduleID: id, Limit: -1})
		if err != nil || len(rs) != 1 || rs[0].Coalesced != 4 || !rs[0].ScheduledFor.Equal(schedT0.Add(5*time.Hour)) {
			t.Errorf("schedule %s: runs = (%+v, %v)", id, rs, err)
		}
		if sc, _ := a.GetSchedule(ctx, id); !sc.NextRunAt.Equal(schedT0.Add(6 * time.Hour)) {
			t.Errorf("schedule %s: next = %v", id, sc.NextRunAt)
		}
	}
}

// TestRunUpdateLocksScheduleFirst: UpdateRun locks the run's schedule
// before the run, as pausing (the schedule, then its open run) does, so an
// auto-pause racing a user's pause cannot deadlock. A transaction holding
// the schedule row stands in for the pause: the auto-pause waits for it
// without holding the run, the pause cancels the run without waiting, and
// the auto-pause then loses its compare-and-swap.
func TestRunUpdateLocksScheduleFirst(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := context.Background()
	sc := schedTestFixture(t, s, "lock order")
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	r, err := s.AddRun(ctx, pendingRun(sc, schedT0), false)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pause.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := pause.Exec(ctx, `UPDATE schedules SET enabled = false, next_run_at = NULL WHERE id = $1`, pgID(sc.ID)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.UpdateRun(ctx, schedules.RunUpdate{
			ID: r.ID, From: schedules.RunPending, To: schedules.RunFailed, Reason: schedules.ReasonWorkerRemoved,
			At: schedT0.Add(time.Minute), PauseSchedule: true,
		})
		done <- err
	}()
	// Wait until the auto-pause waits for a lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("UpdateRun did not wait for the schedule's lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("UpdateRun never waited for the schedule's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The waiting auto-pause holds no lock on the run.
	if _, err := pause.Exec(ctx, `SELECT 1 FROM schedule_runs WHERE id = $1 FOR UPDATE NOWAIT`, pgID(r.ID)); err != nil {
		t.Fatalf("the run is locked while UpdateRun waits for its schedule: %v", err)
	}
	if _, err := pause.Exec(ctx, `UPDATE schedule_runs SET status = 'cancelled', reason = 'disabled' WHERE id = $1`, pgID(r.ID)); err != nil {
		t.Fatal(err)
	}
	if err := pause.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrConflict) {
		t.Errorf("UpdateRun after the pause = %v, want ErrConflict", err)
	}
	if got, _ := s.GetRun(ctx, r.ID); got.Status != schedules.RunCancelled {
		t.Errorf("run = %+v, want cancelled by the pause", got)
	}
	if got, _ := s.GetSchedule(ctx, sc.ID); got.PausedReason != "" {
		t.Errorf("the lost auto-pause set the paused reason %q", got.PausedReason)
	}
	// A missing run is still ErrNotFound.
	_, err = s.UpdateRun(ctx, schedules.RunUpdate{ID: domain.NewScheduleRunID(), From: schedules.RunPending, To: schedules.RunMissed, At: schedT0})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateRun(missing) = %v", err)
	}
}
