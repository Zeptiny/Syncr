package storetest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of schedules.Repository (and of the
// jobs.schedule_id link).

func newSchedule(t *testing.T, n int, owner domain.UserID, name string, w domain.WorkerID) schedules.Schedule {
	return schedules.Schedule{
		ID: scheduleID(t, n), OwnerID: owner, Name: name, Description: "every night",
		Cron: "30 2 * * *", Timezone: "Europe/Berlin",
		Kind: "rclone_transfer", Definition: []byte(testDefinition), SearchText: "copy\n/data",
		Target:  schedules.Target{WorkerID: w},
		Missed:  schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: time.Hour},
		Overlap: schedules.OverlapSkip, Enabled: true, NextRunAt: t0().Add(time.Hour),
		CreatedAt: t0(), UpdatedAt: t0(), Version: 1,
	}
}

func newRun(t *testing.T, n int, schedule domain.ScheduleID, status schedules.RunStatus) schedules.Run {
	return schedules.Run{
		ID: runID(t, n), ScheduleID: schedule, Trigger: schedules.TriggerCron,
		ScheduledFor: t0(), Deadline: t0().Add(time.Hour), Status: status,
		CreatedAt: t0(), UpdatedAt: t0(),
	}
}

// requireSchedules creates the users of the test and skips it while the
// schedules port is a stub.
func requireSchedules(t *testing.T, s Store, owners ...domain.UserID) {
	t.Helper()
	for _, o := range owners {
		ensureUser(t, s, o)
	}
	_, err := s.ListSchedules(context.Background(), schedules.ListFilter{})
	skipIfUnsupported(t, "ListSchedules", "track A (schedules)", err)
}

func mustCreateSchedule(t *testing.T, s Store, sc schedules.Schedule) {
	t.Helper()
	if err := s.CreateSchedule(context.Background(), sc); err != nil {
		t.Fatalf("CreateSchedule(%s): %v", sc.Name, err)
	}
}

func mustAddRun(t *testing.T, s Store, r schedules.Run) schedules.Run {
	t.Helper()
	got, err := s.AddRun(context.Background(), r, false)
	if err != nil {
		t.Fatalf("AddRun(%s): %v", r.ID, err)
	}
	return got
}

func mustGetSchedule(t *testing.T, s Store, id domain.ScheduleID) schedules.Schedule {
	t.Helper()
	sc, err := s.GetSchedule(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	return sc
}

func mustGetRun(t *testing.T, s Store, id domain.ScheduleRunID) schedules.Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r
}

// contactsSupported reports whether the store implements the contacts
// port (a schedules adapter may be finished before the contacts one).
func contactsSupported(t *testing.T, s Store) bool {
	t.Helper()
	_, err := s.ListContacts(context.Background(), contacts.ListFilter{})
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ListContacts: %v", err)
	}
	return err == nil
}

func scheduleIDs(ss []schedules.Schedule) []domain.ScheduleID {
	out := make([]domain.ScheduleID, len(ss))
	for i, sc := range ss {
		out[i] = sc.ID
	}
	return out
}

func runIDs(rs []schedules.Run) []domain.ScheduleRunID {
	out := make([]domain.ScheduleRunID, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func testScheduleCreateGetList(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireSchedules(t, s, alice, bob)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)

	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	b := newSchedule(t, 2, alice, "beta", domain.WorkerID{})
	b.Target = schedules.Target{Label: "nas"}
	b.Missed, b.Overlap = schedules.MissedPolicy{Action: schedules.MissedSkip}, schedules.OverlapQueue
	b.Enabled, b.NextRunAt, b.PausedReason = false, time.Time{}, schedules.ReasonWorkerRemoved
	c := newSchedule(t, 3, bob, "nightly", w.ID) // another owner may use the name
	for _, sc := range []schedules.Schedule{a, b, c} {
		mustCreateSchedule(t, s, sc)
	}
	wantErr(t, "CreateSchedule(duplicate id)", s.CreateSchedule(ctx, a), domain.ErrAlreadyExists)
	wantErr(t, "CreateSchedule(name taken, other case)", s.CreateSchedule(ctx, newSchedule(t, 4, alice, "NIGHTLY", w.ID)), domain.ErrAlreadyExists)

	got := mustGetSchedule(t, s, a.ID)
	if got.OwnerID != alice || got.Name != "Nightly" || got.Description != a.Description || got.Cron != a.Cron ||
		got.Timezone != a.Timezone || got.Kind != a.Kind || !sameJSON(got.Definition, a.Definition) ||
		got.SearchText != a.SearchText || got.Target != a.Target || got.Missed != a.Missed || got.Overlap != a.Overlap ||
		!got.Enabled || got.PausedReason != "" || !got.NextRunAt.Equal(a.NextRunAt) || !got.LastFiredAt.IsZero() ||
		!got.CreatedAt.Equal(t0()) || !got.UpdatedAt.Equal(t0()) || got.Version != 1 ||
		len(got.RemoteIDs) != 0 || len(got.Contacts) != 0 {
		t.Errorf("GetSchedule = %+v", got)
	}
	if got := mustGetSchedule(t, s, b.ID); got.Target != b.Target || got.Missed != b.Missed || got.Overlap != b.Overlap ||
		got.Enabled || !got.NextRunAt.IsZero() || got.PausedReason != schedules.ReasonWorkerRemoved {
		t.Errorf("GetSchedule(label target) = %+v", got)
	}
	_, err := s.GetSchedule(ctx, scheduleID(t, 99))
	wantErr(t, "GetSchedule(missing)", err, domain.ErrNotFound)

	some, err := s.GetSchedules(ctx, []domain.ScheduleID{c.ID, a.ID, scheduleID(t, 99), a.ID})
	if err != nil || !slices.Equal(scheduleIDs(some), []domain.ScheduleID{a.ID, c.ID}) {
		t.Errorf("GetSchedules = (%v, %v), want a and c by ID", scheduleIDs(some), err)
	}
	mine, err := s.ListSchedules(ctx, schedules.ListFilter{OwnerID: alice})
	if err != nil || !slices.Equal(scheduleIDs(mine), []domain.ScheduleID{b.ID, a.ID}) {
		t.Errorf("ListSchedules(alice) = (%v, %v), want by lower-cased name", scheduleIDs(mine), err)
	}
	all, err := s.ListSchedules(ctx, schedules.ListFilter{})
	if err != nil || !slices.Equal(scheduleIDs(all), []domain.ScheduleID{b.ID, a.ID, c.ID}) {
		t.Errorf("ListSchedules() = (%v, %v), want by name, then ID", scheduleIDs(all), err)
	}
}

func testScheduleReferences(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)

	// Foreign keys.
	orphan := newSchedule(t, 1, userID(t, 99), "orphan", w.ID)
	wantErr(t, "CreateSchedule(unknown owner)", s.CreateSchedule(ctx, orphan), domain.ErrNotFound)
	noWorker := newSchedule(t, 2, alice, "no worker", workerID(t, 99))
	wantErr(t, "CreateSchedule(unknown worker)", s.CreateSchedule(ctx, noWorker), domain.ErrNotFound)
	noRemote := newSchedule(t, 3, alice, "no remote", w.ID)
	noRemote.RemoteIDs = []domain.RemoteID{remoteID(t, 99)}
	wantErr(t, "CreateSchedule(unknown remote)", s.CreateSchedule(ctx, noRemote), domain.ErrNotFound)
	noContact := newSchedule(t, 4, alice, "no contact", w.ID)
	noContact.Contacts = []schedules.ContactLink{{ContactID: contactID(t, 99), Events: domain.EventSet{domain.EventFailed}}}
	wantErr(t, "CreateSchedule(unknown contact)", s.CreateSchedule(ctx, noContact), domain.ErrNotFound)
	for _, id := range []domain.ScheduleID{orphan.ID, noWorker.ID, noRemote.ID, noContact.ID} {
		if _, err := s.GetSchedule(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("a schedule with a missing reference was stored: %v", err)
		}
	}

	// Checks.
	bad := map[string]func(*schedules.Schedule){
		"worker and label":       func(sc *schedules.Schedule) { sc.Target.Label = "nas" },
		"no target":              func(sc *schedules.Schedule) { sc.Target = schedules.Target{} },
		"disabled with next run": func(sc *schedules.Schedule) { sc.Enabled = false },
		"unknown missed action":  func(sc *schedules.Schedule) { sc.Missed.Action = "later" },
		"negative max delay":     func(sc *schedules.Schedule) { sc.Missed.MaxDelay = -time.Second },
		"unknown overlap":        func(sc *schedules.Schedule) { sc.Overlap = "maybe" },
		"empty name":             func(sc *schedules.Schedule) { sc.Name = "" },
	}
	for name, mutate := range bad {
		sc := newSchedule(t, 10, alice, "checked", w.ID)
		mutate(&sc)
		wantErr(t, "CreateSchedule("+name+")", s.CreateSchedule(ctx, sc), domain.ErrInvalidArgument)
	}

	// A removed worker still exists (history), so a schedule may refer to it.
	removed := newWorker(t, 2)
	mustCreateWorker(t, s, removed)
	if _, err := s.RemoveWorker(ctx, removed.ID, t0()); err != nil {
		t.Fatal(err)
	}
	mustCreateSchedule(t, s, newSchedule(t, 5, alice, "removed worker", removed.ID))

	// Links are stored as sets, by ID.
	r1, r2 := newRemote(t, 1, alice, "Photos"), newRemote(t, 2, alice, "Docs")
	mustCreateRemote(t, s, r1)
	mustCreateRemote(t, s, r2)
	linked := newSchedule(t, 6, alice, "linked", w.ID)
	linked.RemoteIDs = []domain.RemoteID{r2.ID, r1.ID, r2.ID}
	if contactsSupported(t, s) {
		c1, c2 := newContact(t, 1, alice, "Ops"), newContact(t, 2, alice, "Mail")
		mustCreateContact(t, s, c1)
		mustCreateContact(t, s, c2)
		linked.Contacts = []schedules.ContactLink{
			{ContactID: c2.ID, Events: domain.EventSet{domain.EventMissed}},
			{ContactID: c1.ID, Events: domain.EventSet{domain.EventFailed, domain.EventInterrupted}},
		}
		twice := newSchedule(t, 7, alice, "twice", w.ID)
		twice.Contacts = []schedules.ContactLink{linked.Contacts[0], linked.Contacts[0]}
		wantErr(t, "CreateSchedule(contact linked twice)", s.CreateSchedule(ctx, twice), domain.ErrAlreadyExists)
		noEvents := newSchedule(t, 8, alice, "no events", w.ID)
		noEvents.Contacts = []schedules.ContactLink{{ContactID: c1.ID}}
		wantErr(t, "CreateSchedule(link without events)", s.CreateSchedule(ctx, noEvents), domain.ErrInvalidArgument)
		badEvent := newSchedule(t, 9, alice, "bad event", w.ID)
		badEvent.Contacts = []schedules.ContactLink{{ContactID: c1.ID, Events: domain.EventSet{domain.EventTest}}}
		wantErr(t, "CreateSchedule(test event)", s.CreateSchedule(ctx, badEvent), domain.ErrInvalidArgument)
	}
	mustCreateSchedule(t, s, linked)
	got := mustGetSchedule(t, s, linked.ID)
	if !slices.Equal(got.RemoteIDs, []domain.RemoteID{r1.ID, r2.ID}) {
		t.Errorf("RemoteIDs = %v, want the set by ID", got.RemoteIDs)
	}
	if len(linked.Contacts) > 0 {
		if len(got.Contacts) != 2 || got.Contacts[0].ContactID != contactID(t, 1) || got.Contacts[1].ContactID != contactID(t, 2) ||
			!slices.Equal(got.Contacts[0].Events, domain.EventSet{domain.EventFailed, domain.EventInterrupted}) {
			t.Errorf("Contacts = %+v, want by contact ID with their events", got.Contacts)
		}
	}
	if byRemote, _ := s.ListSchedules(ctx, schedules.ListFilter{RemoteID: r1.ID}); !slices.Equal(scheduleIDs(byRemote), []domain.ScheduleID{linked.ID}) {
		t.Errorf("ListSchedules(remote) = %v", scheduleIDs(byRemote))
	}
	if len(linked.Contacts) > 0 {
		if byContact, _ := s.ListSchedules(ctx, schedules.ListFilter{ContactID: contactID(t, 2)}); !slices.Equal(scheduleIDs(byContact), []domain.ScheduleID{linked.ID}) {
			t.Errorf("ListSchedules(contact) = %v", scheduleIDs(byContact))
		}
	}

	// jobs.schedule_id.
	j := newJob(t, 1, w.ID)
	j.ScheduleID = scheduleID(t, 99)
	ensureUser(t, s, j.OwnerID)
	wantErr(t, "CreateJob(unknown schedule)", s.CreateJob(ctx, j), domain.ErrNotFound)
	j.ScheduleID = linked.ID
	mustCreateJob(t, s, j)
	mustCreateJob(t, s, newJob(t, 2, w.ID))
	if got, err := s.GetJob(ctx, j.ID); err != nil || got.ScheduleID != linked.ID {
		t.Errorf("GetJob ScheduleID = (%v, %v)", got.ScheduleID, err)
	}
	next := j
	next.ScheduleID, next.Status = scheduleID(t, 5), domain.JobCancelled
	if got, err := s.UpdateJob(ctx, next); err != nil || got.ScheduleID != linked.ID {
		t.Errorf("UpdateJob wrote ScheduleID: (%v, %v)", got.ScheduleID, err)
	}
	js, err := s.ListJobs(ctx, jobs.ListFilter{ScheduleID: linked.ID, Limit: -1})
	if err != nil || !slices.Equal(jobIDs(js), []domain.JobID{j.ID}) {
		t.Errorf("ListJobs(schedule) = (%v, %v)", jobIDs(js), err)
	}
}

func testScheduleUpdateVersioned(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	mustCreateSchedule(t, s, newSchedule(t, 2, alice, "Taken", w.ID))

	next := a
	next.Name, next.Description, next.Cron, next.Timezone = "Renamed", "", "0 * * * *", "UTC"
	next.Target = schedules.Target{Label: "nas"}
	next.Missed, next.Overlap = schedules.MissedPolicy{Action: schedules.MissedSkip}, schedules.OverlapAllow
	next.Definition, next.SearchText, next.Kind = []byte(`{"rclone_transfer": {"mode": "RCLONE_TRANSFER_MODE_SYNC"}}`), "sync", "rclone_transfer"
	next.NextRunAt, next.UpdatedAt = t0().Add(5*time.Hour), t0().Add(time.Minute)
	// Never written by an edit.
	next.LastFiredAt, next.CreatedAt, next.OwnerID = t0().Add(9*time.Hour), t0().Add(time.Hour), userID(t, 99)
	stored, err := s.UpdateSchedule(ctx, next, false)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Name != "Renamed" || stored.Cron != "0 * * * *" || stored.Target != next.Target ||
		stored.Overlap != schedules.OverlapAllow || stored.SearchText != "sync" || !sameJSON(stored.Definition, next.Definition) ||
		!stored.UpdatedAt.Equal(next.UpdatedAt) {
		t.Errorf("UpdateSchedule returned %+v", stored)
	}
	if !stored.NextRunAt.Equal(a.NextRunAt) || !stored.LastFiredAt.IsZero() || !stored.CreatedAt.Equal(t0()) || stored.OwnerID != alice {
		t.Errorf("UpdateSchedule without reschedule wrote the fire state or immutable fields: %+v", stored)
	}
	next.Version = 2
	if stored, err := s.UpdateSchedule(ctx, next, true); err != nil || !stored.NextRunAt.Equal(t0().Add(5*time.Hour)) || stored.Version != 3 {
		t.Errorf("UpdateSchedule(reschedule) = (%v v%d, %v)", stored.NextRunAt, stored.Version, err)
	}
	stale := a
	stale.Name = "stale"
	_, err = s.UpdateSchedule(ctx, stale, false)
	wantErr(t, "UpdateSchedule(stale version)", err, domain.ErrConflict)
	if got := mustGetSchedule(t, s, a.ID); got.Name != "Renamed" || got.Version != 3 {
		t.Errorf("after a stale update: %q v%d", got.Name, got.Version)
	}
	_, err = s.UpdateSchedule(ctx, newSchedule(t, 9, alice, "missing", w.ID), false)
	wantErr(t, "UpdateSchedule(missing)", err, domain.ErrNotFound)
	taken := mustGetSchedule(t, s, a.ID)
	taken.Name = "TAKEN"
	_, err = s.UpdateSchedule(ctx, taken, false)
	wantErr(t, "UpdateSchedule(name taken)", err, domain.ErrAlreadyExists)
	noWorker := mustGetSchedule(t, s, a.ID)
	noWorker.Target = schedules.Target{WorkerID: workerID(t, 99)}
	_, err = s.UpdateSchedule(ctx, noWorker, false)
	wantErr(t, "UpdateSchedule(unknown worker)", err, domain.ErrNotFound)

	// Disabling keeps no next run (a check) and cancels the open run.
	open := mustAddRun(t, s, newRun(t, 1, a.ID, schedules.RunPending))
	off := mustGetSchedule(t, s, a.ID)
	off.Enabled = false
	_, err = s.UpdateSchedule(ctx, off, false)
	wantErr(t, "UpdateSchedule(disabled with a next run)", err, domain.ErrInvalidArgument)
	off.NextRunAt, off.UpdatedAt = time.Time{}, t0().Add(2*time.Minute)
	if got, err := s.UpdateSchedule(ctx, off, true); err != nil || got.Enabled || !got.NextRunAt.IsZero() {
		t.Fatalf("UpdateSchedule(disable) = (%+v, %v)", got, err)
	}
	if r := mustGetRun(t, s, open.ID); r.Status != schedules.RunCancelled || r.Reason != schedules.ReasonDisabled || !r.UpdatedAt.Equal(off.UpdatedAt) {
		t.Errorf("open run after disabling = %+v", r)
	}
}

func testScheduleSetEnabled(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	open := mustAddRun(t, s, newRun(t, 1, a.ID, schedules.RunPending))
	done := mustAddRun(t, s, newRun(t, 2, a.ID, schedules.RunMissed))

	at := t0().Add(time.Minute)
	got, err := s.SetScheduleEnabled(ctx, a.ID, schedules.Toggle{Enabled: false, NextRunAt: t0().Add(time.Hour), PausedReason: schedules.ReasonWorkerRemoved, At: at})
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || !got.NextRunAt.IsZero() || got.PausedReason != schedules.ReasonWorkerRemoved || got.Version != 2 || !got.UpdatedAt.Equal(at) {
		t.Errorf("SetScheduleEnabled(false) = %+v", got)
	}
	if r := mustGetRun(t, s, open.ID); r.Status != schedules.RunCancelled || r.Reason != schedules.ReasonDisabled || !r.UpdatedAt.Equal(at) {
		t.Errorf("open run after pausing = %+v", r)
	}
	if r := mustGetRun(t, s, done.ID); r.Status != schedules.RunMissed {
		t.Errorf("terminal run after pausing = %+v", r)
	}
	at = at.Add(time.Minute)
	got, err = s.SetScheduleEnabled(ctx, a.ID, schedules.Toggle{Enabled: true, NextRunAt: t0().Add(2 * time.Hour), PausedReason: "ignored", At: at})
	if err != nil || !got.Enabled || !got.NextRunAt.Equal(t0().Add(2*time.Hour)) || got.PausedReason != "" || got.Version != 3 {
		t.Errorf("SetScheduleEnabled(true) = (%+v, %v)", got, err)
	}
	_, err = s.SetScheduleEnabled(ctx, scheduleID(t, 99), schedules.Toggle{At: at})
	wantErr(t, "SetScheduleEnabled(missing)", err, domain.ErrNotFound)
}

func testScheduleFireCAS(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)

	firedAt := a.NextRunAt.Add(5 * time.Second)
	r := newRun(t, 1, a.ID, schedules.RunPending)
	r.ScheduledFor, r.Deadline, r.Coalesced, r.CreatedAt, r.UpdatedAt = a.NextRunAt, a.NextRunAt.Add(time.Hour), 2, firedAt, firedAt
	f := schedules.Fire{ID: a.ID, ExpectedNext: a.NextRunAt, NewNext: a.NextRunAt.Add(24 * time.Hour), Run: r, Merge: true}
	got, err := s.FireSchedule(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || got.Status != schedules.RunPending || got.Coalesced != 2 || !got.ScheduledFor.Equal(r.ScheduledFor) || got.JobStatus != "" {
		t.Errorf("FireSchedule returned %+v", got)
	}
	sc := mustGetSchedule(t, s, a.ID)
	if !sc.NextRunAt.Equal(f.NewNext) || !sc.LastFiredAt.Equal(firedAt) || sc.Version != 1 || !sc.UpdatedAt.Equal(t0()) {
		t.Errorf("after the fire: next %v, last fired %v, v%d", sc.NextRunAt, sc.LastFiredAt, sc.Version)
	}
	// The same fire again: the compare-and-swap misses, nothing changes.
	again := f
	again.Run.ID = runID(t, 2)
	_, err = s.FireSchedule(ctx, again)
	wantErr(t, "FireSchedule(stale next run)", err, domain.ErrConflict)
	if _, err := s.GetRun(ctx, runID(t, 2)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a stale fire stored its run: %v", err)
	}
	if got := mustGetSchedule(t, s, a.ID); !got.NextRunAt.Equal(f.NewNext) {
		t.Errorf("a stale fire moved the next run to %v", got.NextRunAt)
	}
	// A paused schedule does not fire.
	if _, err := s.SetScheduleEnabled(ctx, a.ID, schedules.Toggle{Enabled: false, At: firedAt}); err != nil {
		t.Fatal(err)
	}
	paused := again
	paused.ExpectedNext = time.Time{}
	_, err = s.FireSchedule(ctx, paused)
	wantErr(t, "FireSchedule(paused)", err, domain.ErrConflict)
	missing := again
	missing.ID = scheduleID(t, 99)
	_, err = s.FireSchedule(ctx, missing)
	wantErr(t, "FireSchedule(missing)", err, domain.ErrNotFound)
	// No further occurrence: the next run becomes empty.
	b := newSchedule(t, 2, alice, "Once", w.ID)
	mustCreateSchedule(t, s, b)
	last := newRun(t, 3, b.ID, schedules.RunMissed)
	last.Reason = schedules.ReasonOverlap
	if _, err := s.FireSchedule(ctx, schedules.Fire{ID: b.ID, ExpectedNext: b.NextRunAt, Run: last}); err != nil {
		t.Fatal(err)
	}
	if got := mustGetSchedule(t, s, b.ID); !got.NextRunAt.IsZero() || !got.Enabled {
		t.Errorf("after the last fire: next %v enabled %v", got.NextRunAt, got.Enabled)
	}
}

// testScheduleFireRace fires one occurrence from many goroutines: exactly
// one fire wins and one run exists.
func testScheduleFireRace(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 16 {
		wg.Go(func() {
			r := newRun(t, 100+i, a.ID, schedules.RunPending)
			_, err := s.FireSchedule(ctx, schedules.Fire{ID: a.ID, ExpectedNext: a.NextRunAt, NewNext: a.NextRunAt.Add(24 * time.Hour), Run: r, Merge: true})
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
		t.Errorf("%d concurrent fires of one occurrence succeeded, want 1", wins)
	}
	if rs, err := s.ListRuns(ctx, schedules.RunFilter{ScheduleID: a.ID, Limit: -1}); err != nil || len(rs) != 1 || rs[0].Coalesced != 0 {
		t.Errorf("runs after the race = (%+v, %v), want one", rs, err)
	}
}

func testScheduleMergeOpenRun(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	day := 24 * time.Hour
	fire := func(expected time.Time, r schedules.Run, merge bool) (schedules.Run, error) {
		return s.FireSchedule(ctx, schedules.Fire{ID: a.ID, ExpectedNext: expected, NewNext: expected.Add(day), Run: r, Merge: merge})
	}
	first := newRun(t, 1, a.ID, schedules.RunPending)
	if _, err := fire(a.NextRunAt, first, true); err != nil {
		t.Fatal(err)
	}
	// A later occurrence merges into the open run.
	second := newRun(t, 2, a.ID, schedules.RunPending)
	second.Coalesced, second.Deadline, second.CreatedAt = 2, t0().Add(30*time.Hour), t0().Add(day)
	merged, err := fire(a.NextRunAt.Add(day), second, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.ID != first.ID || merged.Coalesced != 3 || !merged.Deadline.Equal(second.Deadline) ||
		!merged.UpdatedAt.Equal(second.CreatedAt) || !merged.ScheduledFor.Equal(first.ScheduledFor) {
		t.Errorf("merged run = %+v", merged)
	}
	if _, err := s.GetRun(ctx, second.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the merged run was stored too: %v", err)
	}
	// An earlier deadline does not shorten the open run's.
	third := newRun(t, 3, a.ID, schedules.RunPending)
	third.Deadline = t0()
	if merged, err = fire(a.NextRunAt.Add(2*day), third, true); err != nil || !merged.Deadline.Equal(second.Deadline) || merged.Coalesced != 4 {
		t.Errorf("merge with an earlier deadline = (%+v, %v)", merged, err)
	}
	// Without merge, a second open run violates the one-open-run rule and
	// the fire changes nothing.
	expected := a.NextRunAt.Add(3 * day)
	_, err = fire(expected, newRun(t, 4, a.ID, schedules.RunPending), false)
	wantErr(t, "FireSchedule(second open run)", err, domain.ErrAlreadyExists)
	if got := mustGetSchedule(t, s, a.ID); !got.NextRunAt.Equal(expected) {
		t.Errorf("a failed fire moved the next run to %v", got.NextRunAt)
	}
	// A terminal run is always inserted.
	skipped := newRun(t, 5, a.ID, schedules.RunMissed)
	skipped.Reason = schedules.ReasonOverlap
	if got, err := fire(expected, skipped, false); err != nil || got.ID != skipped.ID || got.Status != schedules.RunMissed {
		t.Errorf("FireSchedule(missed run) = (%+v, %v)", got, err)
	}

	// AddRun (manual) merges or refuses the same way, without the fire
	// state.
	before := mustGetSchedule(t, s, a.ID)
	manual := newRun(t, 6, a.ID, schedules.RunPending)
	manual.Trigger = schedules.TriggerManual
	_, err = s.AddRun(ctx, manual, false)
	wantErr(t, "AddRun(second open run)", err, domain.ErrAlreadyExists)
	if got, err := s.AddRun(ctx, manual, true); err != nil || got.ID != first.ID || got.Coalesced != 5 {
		t.Errorf("AddRun(merge) = (%+v, %v)", got, err)
	}
	if got := mustGetSchedule(t, s, a.ID); !got.NextRunAt.Equal(before.NextRunAt) || !got.LastFiredAt.Equal(before.LastFiredAt) {
		t.Error("AddRun changed the fire state")
	}
	orphan := newRun(t, 7, scheduleID(t, 99), schedules.RunPending)
	_, err = s.AddRun(ctx, orphan, false)
	wantErr(t, "AddRun(unknown schedule)", err, domain.ErrNotFound)
	dup := newRun(t, 5, a.ID, schedules.RunMissed)
	_, err = s.AddRun(ctx, dup, false)
	wantErr(t, "AddRun(duplicate id)", err, domain.ErrAlreadyExists)
	badTrigger := newRun(t, 8, a.ID, schedules.RunMissed)
	badTrigger.Trigger = "timer"
	_, err = s.AddRun(ctx, badTrigger, false)
	wantErr(t, "AddRun(unknown trigger)", err, domain.ErrInvalidArgument)
	noJob := newRun(t, 9, a.ID, schedules.RunLaunched)
	_, err = s.AddRun(ctx, noJob, false)
	wantErr(t, "AddRun(launched without a job)", err, domain.ErrInvalidArgument)
}

// testScheduleOnePendingRun: a schedule has at most one pending run, but a
// launching run absorbs no fire: an occurrence due while a run launches
// opens a pending run of its own.
func testScheduleOnePendingRun(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	day := 24 * time.Hour
	at := t0().Add(time.Minute)
	launch := func(r schedules.Run, job int) schedules.Run {
		t.Helper()
		got, err := s.UpdateRun(ctx, schedules.RunUpdate{
			ID: r.ID, From: schedules.RunPending, To: schedules.RunLaunching, WorkerID: w.ID, JobID: jobID(t, job), At: at,
		})
		if err != nil {
			t.Fatalf("UpdateRun(launching): %v", err)
		}
		return got
	}

	first, err := s.FireSchedule(ctx, schedules.Fire{
		ID: a.ID, ExpectedNext: a.NextRunAt, NewNext: a.NextRunAt.Add(day), Run: newRun(t, 1, a.ID, schedules.RunPending), Merge: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first = launch(first, 1)
	// The next occurrence fires while the first run launches: a new run.
	second := newRun(t, 2, a.ID, schedules.RunPending)
	second.CreatedAt = t0().Add(day)
	got, err := s.FireSchedule(ctx, schedules.Fire{
		ID: a.ID, ExpectedNext: a.NextRunAt.Add(day), NewNext: a.NextRunAt.Add(2 * day), Run: second, Merge: true,
	})
	if err != nil || got.ID != second.ID || got.Status != schedules.RunPending || got.Coalesced != 0 {
		t.Fatalf("FireSchedule while a run launches = (%+v, %v), want the new pending run", got, err)
	}
	if r := mustGetRun(t, s, first.ID); r.Status != schedules.RunLaunching || r.Coalesced != 0 {
		t.Errorf("the launching run absorbed the fire: %+v", r)
	}
	// A manual request merges into the pending run, not the launching one.
	manual := newRun(t, 3, a.ID, schedules.RunPending)
	manual.Trigger = schedules.TriggerManual
	if got, err := s.AddRun(ctx, manual, true); err != nil || got.ID != second.ID || got.Coalesced != 1 {
		t.Errorf("AddRun(merge) = (%+v, %v), want merged into the pending run", got, err)
	}
	// Still at most one pending run.
	_, err = s.AddRun(ctx, newRun(t, 4, a.ID, schedules.RunPending), false)
	wantErr(t, "AddRun(second pending run)", err, domain.ErrAlreadyExists)
	_, err = s.UpdateRun(ctx, schedules.RunUpdate{ID: first.ID, From: schedules.RunLaunching, To: schedules.RunPending, At: at})
	wantErr(t, "UpdateRun(launching -> pending beside a pending run)", err, domain.ErrAlreadyExists)
	if r := mustGetRun(t, s, first.ID); r.Status != schedules.RunLaunching {
		t.Errorf("a refused update changed the run: %+v", r)
	}
	// Several runs may launch at once (overlap allow).
	launch(mustGetRun(t, s, second.ID), 2)
	open, err := s.ListOpenRuns(ctx, -1)
	if err != nil || !slices.Equal(runIDs(open), []domain.ScheduleRunID{first.ID, second.ID}) {
		t.Errorf("ListOpenRuns = (%v, %v), want both launching runs", runIDs(open), err)
	}
	third := mustAddRun(t, s, newRun(t, 5, a.ID, schedules.RunPending))

	// Pausing cancels every open run.
	if _, err := s.SetScheduleEnabled(ctx, a.ID, schedules.Toggle{Enabled: false, At: at}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.ScheduleRunID{first.ID, second.ID, third.ID} {
		if r := mustGetRun(t, s, id); r.Status != schedules.RunCancelled || r.Reason != schedules.ReasonDisabled {
			t.Errorf("run %s after pausing = %+v", id, r)
		}
	}
}

func testScheduleRunCAS(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	mustCreateSchedule(t, s, a)
	r := mustAddRun(t, s, newRun(t, 1, a.ID, schedules.RunPending))
	at := t0().Add(time.Minute)

	_, err := s.UpdateRun(ctx, schedules.RunUpdate{ID: r.ID, From: schedules.RunPending, To: schedules.RunLaunching, At: at})
	wantErr(t, "UpdateRun(launching without a job)", err, domain.ErrInvalidArgument)
	if got := mustGetRun(t, s, r.ID); got.Status != schedules.RunPending {
		t.Errorf("a refused update changed the run: %+v", got)
	}

	// Concurrent launches reserve one job ID.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []domain.JobID
	for i := range 10 {
		wg.Go(func() {
			got, err := s.UpdateRun(ctx, schedules.RunUpdate{
				ID: r.ID, From: schedules.RunPending, To: schedules.RunLaunching, WorkerID: w.ID, JobID: jobID(t, i), At: at,
			})
			switch {
			case err == nil:
				mu.Lock()
				winners = append(winners, got.JobID)
				mu.Unlock()
			case !errors.Is(err, domain.ErrConflict):
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d concurrent launches succeeded, want 1", len(winners))
	}
	launching := mustGetRun(t, s, r.ID)
	if launching.Status != schedules.RunLaunching || launching.JobID != winners[0] || launching.WorkerID != w.ID ||
		!launching.UpdatedAt.Equal(at) || launching.JobStatus != "" {
		t.Errorf("launching run = %+v", launching)
	}
	// The job does not exist yet: the reserved ID is not a foreign key.
	j := newJob(t, 0, w.ID)
	j.ID, j.ScheduleID = winners[0], a.ID
	mustCreateJob(t, s, j)
	if got := mustGetRun(t, s, r.ID); got.JobStatus != domain.JobQueued {
		t.Errorf("JobStatus = %q, want queued", got.JobStatus)
	}
	// Zero IDs keep the stored ones; a stale From is a conflict.
	got, err := s.UpdateRun(ctx, schedules.RunUpdate{ID: r.ID, From: schedules.RunLaunching, To: schedules.RunLaunched, At: at.Add(time.Second)})
	if err != nil || got.Status != schedules.RunLaunched || got.JobID != winners[0] || got.WorkerID != w.ID || got.JobStatus != domain.JobQueued {
		t.Errorf("UpdateRun(launched) = (%+v, %v)", got, err)
	}
	_, err = s.UpdateRun(ctx, schedules.RunUpdate{ID: r.ID, From: schedules.RunLaunching, To: schedules.RunFailed, At: at})
	wantErr(t, "UpdateRun(stale status)", err, domain.ErrConflict)
	_, err = s.UpdateRun(ctx, schedules.RunUpdate{ID: runID(t, 99), From: schedules.RunPending, To: schedules.RunMissed, At: at})
	wantErr(t, "UpdateRun(missing)", err, domain.ErrNotFound)

	// At most one open run.
	open := mustAddRun(t, s, newRun(t, 2, a.ID, schedules.RunPending))
	missed := mustAddRun(t, s, newRun(t, 3, a.ID, schedules.RunMissed))
	_, err = s.UpdateRun(ctx, schedules.RunUpdate{ID: missed.ID, From: schedules.RunMissed, To: schedules.RunPending, At: at})
	wantErr(t, "UpdateRun(second open run)", err, domain.ErrAlreadyExists)

	// Ending a run may pause its schedule in the same step.
	ended, err := s.UpdateRun(ctx, schedules.RunUpdate{
		ID: open.ID, From: schedules.RunPending, To: schedules.RunFailed, Reason: schedules.ReasonWorkerRemoved,
		Message: "the worker was removed", At: at, PauseSchedule: true,
	})
	if err != nil || ended.Status != schedules.RunFailed || ended.Reason != schedules.ReasonWorkerRemoved || ended.Message == "" {
		t.Errorf("UpdateRun(pause) = (%+v, %v)", ended, err)
	}
	if sc := mustGetSchedule(t, s, a.ID); sc.Enabled || !sc.NextRunAt.IsZero() || sc.PausedReason != schedules.ReasonWorkerRemoved || sc.Version != 2 {
		t.Errorf("schedule after an auto-pause = %+v", sc)
	}
}

func testScheduleHasActiveRun(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	b := newSchedule(t, 2, alice, "Other", w.ID)
	mustCreateSchedule(t, s, a)
	mustCreateSchedule(t, s, b)
	active := func(want bool, what string) {
		t.Helper()
		if got, err := s.HasActiveRun(ctx, a.ID); err != nil || got != want {
			t.Errorf("%s: HasActiveRun = (%v, %v), want %v", what, got, err, want)
		}
	}
	active(false, "nothing")
	pending := mustAddRun(t, s, newRun(t, 1, a.ID, schedules.RunPending))
	active(false, "a pending run")
	r := mustAddRun(t, s, newRun(t, 2, b.ID, schedules.RunPending))
	if _, err := s.UpdateRun(ctx, schedules.RunUpdate{ID: r.ID, From: schedules.RunPending, To: schedules.RunLaunching, JobID: jobID(t, 9), At: t0()}); err != nil {
		t.Fatal(err)
	}
	active(false, "another schedule's launching run")

	mustAddRun(t, s, newRun(t, 3, a.ID, schedules.RunMissed))
	active(false, "a missed run")
	if _, err := s.UpdateRun(ctx, schedules.RunUpdate{ID: pending.ID, From: schedules.RunPending, To: schedules.RunLaunching, JobID: jobID(t, 1), At: t0()}); err != nil {
		t.Fatal(err)
	}
	active(true, "a launching run")
	if _, err := s.UpdateRun(ctx, schedules.RunUpdate{ID: pending.ID, From: schedules.RunLaunching, To: schedules.RunLaunched, At: t0()}); err != nil {
		t.Fatal(err)
	}
	active(false, "a launched run whose job does not exist")
	j := newJob(t, 1, w.ID)
	j.ScheduleID = a.ID
	mustCreateJob(t, s, j)
	for _, st := range []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning} {
		cur, err := s.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != st {
			cur.Status = st
			if _, err := s.UpdateJob(ctx, cur); err != nil {
				t.Fatal(err)
			}
		}
		active(true, "a "+string(st)+" job")
	}
	cur, _ := s.GetJob(ctx, j.ID)
	cur.Status, cur.FinishedAt = domain.JobSucceeded, t0()
	if _, err := s.UpdateJob(ctx, cur); err != nil {
		t.Fatal(err)
	}
	active(false, "a finished job")
}

func testScheduleLists(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireSchedules(t, s, alice, bob)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "a", w.ID) // next t0+1h
	b := newSchedule(t, 2, alice, "b", w.ID)
	b.NextRunAt = t0().Add(-time.Hour)
	c := newSchedule(t, 3, alice, "c", w.ID)
	c.Enabled, c.NextRunAt = false, time.Time{}
	d := newSchedule(t, 4, bob, "d", w.ID)
	d.NextRunAt = t0().Add(30 * time.Minute)
	e := newSchedule(t, 5, bob, "e", w.ID)
	e.NextRunAt = t0().Add(30 * time.Minute) // same time as d: by ID
	for _, sc := range []schedules.Schedule{a, b, c, d, e} {
		mustCreateSchedule(t, s, sc)
	}
	due, err := s.ListDueSchedules(ctx, t0().Add(time.Hour), -1)
	if err != nil || !slices.Equal(scheduleIDs(due), []domain.ScheduleID{b.ID, d.ID, e.ID, a.ID}) {
		t.Errorf("ListDueSchedules = (%v, %v)", scheduleIDs(due), err)
	}
	if due, _ := s.ListDueSchedules(ctx, t0(), 10); !slices.Equal(scheduleIDs(due), []domain.ScheduleID{b.ID}) {
		t.Errorf("ListDueSchedules(t0) = %v", scheduleIDs(due))
	}
	if due, _ := s.ListDueSchedules(ctx, t0().Add(time.Hour), 2); !slices.Equal(scheduleIDs(due), []domain.ScheduleID{b.ID, d.ID}) {
		t.Errorf("ListDueSchedules(limit 2) = %v", scheduleIDs(due))
	}
	up, err := s.ListUpcoming(ctx, alice, 10)
	if err != nil || len(up) != 2 || up[0].ScheduleID != b.ID || up[1].ScheduleID != a.ID ||
		up[1].Name != "a" || !up[1].At.Equal(a.NextRunAt) || up[1].Target != a.Target {
		t.Errorf("ListUpcoming(alice) = (%+v, %v)", up, err)
	}
	if up, _ := s.ListUpcoming(ctx, domain.UserID{}, 3); len(up) != 3 || up[0].ScheduleID != b.ID || up[2].ScheduleID != e.ID {
		t.Errorf("ListUpcoming(all, 3) = %+v", up)
	}

	// Runs: r1..r5 over a and d.
	mk := func(n int, sc domain.ScheduleID, st schedules.RunStatus, scheduled time.Time) schedules.Run {
		r := newRun(t, n, sc, st)
		r.ScheduledFor = scheduled
		return mustAddRun(t, s, r)
	}
	r1 := mk(1, a.ID, schedules.RunMissed, t0().Add(-3*time.Hour))
	r2 := mk(2, a.ID, schedules.RunFailed, t0().Add(-2*time.Hour))
	r3 := mk(3, a.ID, schedules.RunPending, t0().Add(-time.Hour))
	r4 := mk(4, d.ID, schedules.RunPending, t0().Add(-4*time.Hour))
	r5 := mk(5, d.ID, schedules.RunMissed, t0())
	open, err := s.ListOpenRuns(ctx, -1)
	if err != nil || !slices.Equal(runIDs(open), []domain.ScheduleRunID{r4.ID, r3.ID}) {
		t.Errorf("ListOpenRuns = (%v, %v), want by scheduled time", runIDs(open), err)
	}
	if open, _ := s.ListOpenRuns(ctx, 1); len(open) != 1 {
		t.Errorf("ListOpenRuns(1) = %v", runIDs(open))
	}
	list := func(f schedules.RunFilter) []domain.ScheduleRunID {
		t.Helper()
		rs, err := s.ListRuns(ctx, f)
		if err != nil {
			t.Fatalf("ListRuns(%+v): %v", f, err)
		}
		return runIDs(rs)
	}
	problems := []schedules.RunStatus{schedules.RunMissed, schedules.RunFailed}
	for name, tt := range map[string]struct {
		f    schedules.RunFilter
		want []domain.ScheduleRunID
	}{
		"schedule":        {schedules.RunFilter{ScheduleID: a.ID, Limit: -1}, []domain.ScheduleRunID{r3.ID, r2.ID, r1.ID}},
		"all":             {schedules.RunFilter{Limit: -1}, []domain.ScheduleRunID{r5.ID, r4.ID, r3.ID, r2.ID, r1.ID}},
		"owner problems":  {schedules.RunFilter{OwnerID: alice, Statuses: problems, Limit: -1}, []domain.ScheduleRunID{r2.ID, r1.ID}},
		"scheduled range": {schedules.RunFilter{ScheduledFrom: t0().Add(-3 * time.Hour), ScheduledUntil: t0(), Limit: -1}, []domain.ScheduleRunID{r3.ID, r2.ID, r1.ID}},
		"page":            {schedules.RunFilter{Before: r4.ID, Limit: 2}, []domain.ScheduleRunID{r3.ID, r2.ID}},
	} {
		if got := list(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("ListRuns(%s) = %v, want %v", name, got, tt.want)
		}
	}
	if n, err := s.CountRuns(ctx, schedules.RunFilter{Statuses: []schedules.RunStatus{schedules.RunMissed}, Before: r1.ID, Limit: 1}); err != nil || n != 2 {
		t.Errorf("CountRuns(missed) = (%d, %v), want 2 (Before and Limit ignored)", n, err)
	}
	if n, _ := s.CountRuns(ctx, schedules.RunFilter{OwnerID: alice, ScheduledFrom: t0().Add(-2 * time.Hour)}); n != 2 {
		t.Errorf("CountRuns(alice, since -2h) = %d", n)
	}
	latest, err := s.LatestRuns(ctx, []domain.ScheduleID{a.ID, d.ID, c.ID})
	if err != nil || len(latest) != 2 || latest[a.ID].ID != r3.ID || latest[d.ID].ID != r5.ID {
		t.Errorf("LatestRuns = (%v, %v)", latest, err)
	}
}

func testScheduleDeleteCascades(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	keep := newSchedule(t, 2, alice, "Keep", w.ID)
	mustCreateSchedule(t, s, a)
	mustCreateSchedule(t, s, keep)
	r := mustAddRun(t, s, newRun(t, 1, a.ID, schedules.RunMissed))
	other := mustAddRun(t, s, newRun(t, 2, keep.ID, schedules.RunMissed))
	j := newJob(t, 1, w.ID)
	j.ScheduleID = a.ID
	mustCreateJob(t, s, j)

	if err := s.DeleteSchedule(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSchedule(ctx, a.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetSchedule(deleted) = %v", err)
	}
	if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the deleted schedule's run survived: %v", err)
	}
	if got, err := s.GetJob(ctx, j.ID); err != nil || !got.ScheduleID.IsZero() {
		t.Errorf("the deleted schedule's job = (%v, %v), want kept with no schedule", got.ScheduleID, err)
	}
	mustGetRun(t, s, other.ID)
	wantErr(t, "DeleteSchedule(deleted)", s.DeleteSchedule(ctx, a.ID), domain.ErrNotFound)

	// A user who owns schedules cannot be deleted.
	u := newUser(t, 10)
	mustCreateUser(t, s, u)
	mustCreateSchedule(t, s, newSchedule(t, 3, u.ID, "Mine", w.ID))
	wantErr(t, "DeleteUser(owns a schedule)", s.DeleteUser(ctx, u.ID), domain.ErrFailedPrecondition)
}

func testScheduleNoAliasing(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	r := newRemote(t, 1, alice, "Photos")
	mustCreateRemote(t, s, r)
	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	a.RemoteIDs = []domain.RemoteID{r.ID}
	mustCreateSchedule(t, s, a)
	a.RemoteIDs[0], a.Definition[0] = remoteID(t, 99), 'x'

	got := mustGetSchedule(t, s, a.ID)
	if got.RemoteIDs[0] != r.ID || !sameJSON(got.Definition, []byte(testDefinition)) {
		t.Fatalf("CreateSchedule aliases its argument: %+v", got)
	}
	got.RemoteIDs[0], got.Definition[0] = remoteID(t, 98), 'y'
	if again := mustGetSchedule(t, s, a.ID); again.RemoteIDs[0] != r.ID || !sameJSON(again.Definition, []byte(testDefinition)) {
		t.Error("GetSchedule returns storage")
	}
	list, _ := s.ListSchedules(ctx, schedules.ListFilter{})
	list[0].RemoteIDs[0] = remoteID(t, 97)
	if again := mustGetSchedule(t, s, a.ID); again.RemoteIDs[0] != r.ID {
		t.Error("ListSchedules returns storage")
	}
}
