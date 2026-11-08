package schedules_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestNewRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := schedules.New(schedules.Deps{}, schedules.Config{}); err == nil {
		t.Error("New accepted missing dependencies")
	}
	e := newEnv(t)
	d := schedules.Deps{Repo: e.store, Remotes: e.remotes, Contacts: e.contacts, Workers: storeWorkers{e.store}, Waker: e.waker, Clock: e.clock, IDs: nil}
	if _, err := schedules.New(d, schedules.Config{}); err == nil {
		t.Error("New accepted a nil ID generator")
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r1, r2 := e.addRemote(e.alice, "photos"), e.addRemote(e.alice, "docs")
	c1, c2 := e.addContact(e.alice, "ops"), e.addContact(e.alice, "mail")
	in := e.input("  Nightly  ")
	in.Cron, in.Timezone = "  30   2 * * MON-FRI ", "Europe/Berlin"
	in.RemoteIDs = []domain.RemoteID{r2, r1, r2}
	in.Overlap = ""
	in.Contacts = []schedules.ContactLink{
		{ContactID: c2, Events: domain.EventSet{domain.EventMissed, domain.EventFailed}},
		{ContactID: c1, Events: domain.EventSet{domain.EventSucceeded}},
	}
	before := e.waker.n.Load()
	sc, err := e.svc.Create(ctx, e.alice, in)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-04 is a Wednesday: the next 02:30 CET is on Thursday.
	wantNext := time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC)
	if sc.Name != "Nightly" || sc.Cron != "30 2 * * mon-fri" || sc.OwnerID != e.alice.UserID || sc.Version != 1 ||
		!sc.NextRunAt.Equal(wantNext) || !sc.LastFiredAt.IsZero() || sc.Overlap != schedules.OverlapSkip ||
		sc.SearchText != "copy\n/data\n/backup" || !sc.CreatedAt.Equal(t0) || !sc.UpdatedAt.Equal(t0) {
		t.Errorf("Create = %+v", sc)
	}
	if len(sc.RemoteIDs) != 2 || len(sc.Contacts) != 2 {
		t.Fatalf("links = %v %v", sc.RemoteIDs, sc.Contacts)
	}
	for _, l := range sc.Contacts {
		if l.ContactID == c2 && !slices.Equal(l.Events, domain.EventSet{domain.EventFailed, domain.EventMissed}) {
			t.Errorf("events of c2 = %v, want normalized", l.Events)
		}
	}
	if got := e.schedule(sc.ID); !slices.Equal(got.RemoteIDs, sc.RemoteIDs) || got.Name != "Nightly" {
		t.Errorf("stored = %+v", got)
	}
	if e.waker.n.Load() != before+1 {
		t.Error("Create did not wake the scheduler")
	}

	// A label target is lower-cased; a disabled schedule has no next run.
	lab := e.input("by label")
	lab.Target, lab.Enabled = schedules.Target{Label: "NAS"}, false
	lab.Missed = schedules.MissedPolicy{Action: schedules.MissedSkip, MaxDelay: time.Hour}
	got := e.create(e.alice, lab)
	if got.Target.Label != "nas" || !got.NextRunAt.IsZero() || got.Missed.MaxDelay != 0 {
		t.Errorf("Create(label, disabled) = %+v", got)
	}

	_, err = e.svc.Create(ctx, e.alice, e.input("NIGHTLY"))
	wantErr(t, "Create(name taken)", err, domain.ErrAlreadyExists)
	_, err = e.svc.Create(ctx, domain.Actor{}, e.input("anonymous"))
	wantErr(t, "Create(no user)", err, domain.ErrPermissionDenied)
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	removed := e.addWorker("gone")
	if _, err := e.store.RemoveWorker(context.Background(), removed, t0); err != nil {
		t.Fatal(err)
	}
	c := e.addContact(e.alice, "ops")
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'x'
	}
	tests := []struct {
		name   string
		mutate func(*schedules.Input)
		want   []string
	}{
		{"no name", func(in *schedules.Input) { in.Name = " " }, []string{"name"}},
		{"long name", func(in *schedules.Input) { in.Name = string(long) }, []string{"name"}},
		{"control in name", func(in *schedules.Input) { in.Name = "a\tb" }, []string{"name"}},
		{"bad cron", func(in *schedules.Input) { in.Cron = "@every 5m" }, []string{"cron"}},
		{"seconds", func(in *schedules.Input) { in.Cron = "0 0 2 * * *" }, []string{"cron"}},
		{"never", func(in *schedules.Input) { in.Cron = "0 0 30 2 *" }, []string{"cron"}},
		{"local zone", func(in *schedules.Input) { in.Timezone = "Local" }, []string{"timezone"}},
		{"no target", func(in *schedules.Input) { in.Target = schedules.Target{} }, []string{"target"}},
		{"both targets", func(in *schedules.Input) { in.Target.Label = "nas" }, []string{"target"}},
		{"bad label", func(in *schedules.Input) { in.Target = schedules.Target{Label: "-nas"} }, []string{"target.worker_label"}},
		{"unknown worker", func(in *schedules.Input) { in.Target = schedules.Target{WorkerID: domain.NewWorkerID()} }, []string{"target.worker_id"}},
		{"removed worker", func(in *schedules.Input) { in.Target = schedules.Target{WorkerID: removed} }, []string{"target.worker_id"}},
		{"negative delay", func(in *schedules.Input) { in.Missed.MaxDelay = -time.Second }, []string{"missed_run.max_delay"}},
		{"delay over a week", func(in *schedules.Input) { in.Missed.MaxDelay = schedules.MaxMaxDelay + time.Second }, []string{"missed_run.max_delay"}},
		{"unknown action", func(in *schedules.Input) { in.Missed.Action = "later" }, []string{"missed_run.action"}},
		{"unknown overlap", func(in *schedules.Input) { in.Overlap = "maybe" }, []string{"overlap"}},
		{"no definition", func(in *schedules.Input) { in.Definition = nil }, []string{"definition"}},
		{"zero remote", func(in *schedules.Input) { in.RemoteIDs = []domain.RemoteID{{}} }, []string{"definition"}},
		{"zero contact", func(in *schedules.Input) {
			in.Contacts = []schedules.ContactLink{{Events: domain.EventSet{domain.EventFailed}}}
		}, []string{"contacts[0].contact_id"}},
		{"no events", func(in *schedules.Input) { in.Contacts = []schedules.ContactLink{{ContactID: c}} }, []string{"contacts[0].events"}},
		{"test event", func(in *schedules.Input) {
			in.Contacts = []schedules.ContactLink{{ContactID: c, Events: domain.EventSet{domain.EventTest}}}
		}, []string{"contacts[0].events"}},
		{"contact twice", func(in *schedules.Input) {
			l := schedules.ContactLink{ContactID: c, Events: domain.EventSet{domain.EventFailed}}
			in.Contacts = []schedules.ContactLink{l, l}
		}, []string{"contacts[1].contact_id"}},
		{"several at once", func(in *schedules.Input) { in.Name, in.Cron, in.Timezone = "", "nope", "Nowhere/City" }, []string{"name", "timezone", "cron"}},
	}
	for _, tt := range tests {
		in := e.input("checked")
		tt.mutate(&in)
		_, err := e.svc.Create(context.Background(), e.alice, in)
		if err == nil {
			t.Errorf("%s: Create accepted it", tt.name)
			continue
		}
		if got := fieldsOf(t, err); !slices.Equal(got, tt.want) {
			t.Errorf("%s: fields = %v, want %v (%v)", tt.name, got, tt.want, err)
		}
	}
	if ss, _ := e.store.ListSchedules(context.Background(), schedules.ListFilter{}); len(ss) != 0 {
		t.Errorf("invalid inputs stored %d schedules", len(ss))
	}
}

// TestOwnerScoping: schedules are private to their owner, admins included;
// another user's schedule, remote or contact is not found.
func TestOwnerScoping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("mine"))
	run, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []domain.Actor{e.bob, e.admin} {
		_, err := e.svc.Get(ctx, other, sc.ID)
		wantErr(t, "Get", err, domain.ErrNotFound)
		_, err = e.svc.Update(ctx, other, sc.ID, sc.Version, e.input("theirs"))
		wantErr(t, "Update", err, domain.ErrNotFound)
		_, err = e.svc.SetEnabled(ctx, other, sc.ID, false)
		wantErr(t, "SetEnabled", err, domain.ErrNotFound)
		_, err = e.svc.RunNow(ctx, other, sc.ID)
		wantErr(t, "RunNow", err, domain.ErrNotFound)
		_, err = e.svc.Runs(ctx, other, sc.ID, domain.ScheduleRunID{}, 0)
		wantErr(t, "Runs", err, domain.ErrNotFound)
		wantErr(t, "Delete", e.svc.Delete(ctx, other, sc.ID), domain.ErrNotFound)
		if list, err := e.svc.List(ctx, other, schedules.ListFilter{}); err != nil || len(list) != 0 {
			t.Errorf("List(%s) = (%v, %v)", other.Role, list, err)
		}
		if latest, err := e.svc.LatestRuns(ctx, other, []domain.ScheduleID{sc.ID}); err != nil || len(latest) != 0 {
			t.Errorf("LatestRuns(%s) = (%v, %v)", other.Role, latest, err)
		}
		if up, _ := e.svc.Upcoming(ctx, other, 0); len(up) != 0 {
			t.Errorf("Upcoming(%s) = %v", other.Role, up)
		}
	}
	if latest, _ := e.svc.LatestRuns(ctx, e.alice, []domain.ScheduleID{sc.ID}); latest[sc.ID].ID != run.ID {
		t.Errorf("LatestRuns(owner) = %v", latest)
	}
	if _, err := e.svc.Get(ctx, e.alice, sc.ID); err != nil {
		t.Errorf("Get(owner): %v", err)
	}

	// Linking someone else's remote or contact.
	bobsRemote, bobsContact := e.addRemote(e.bob, "bobs"), e.addContact(e.bob, "bobs")
	in := e.input("borrowed remote")
	in.RemoteIDs = []domain.RemoteID{bobsRemote}
	_, err = e.svc.Create(ctx, e.alice, in)
	wantErr(t, "Create(another user's remote)", err, schedules.ErrUnknownRemote)
	in = e.input("borrowed contact")
	in.Contacts = []schedules.ContactLink{{ContactID: bobsContact, Events: domain.EventSet{domain.EventFailed}}}
	_, err = e.svc.Create(ctx, e.alice, in)
	wantErr(t, "Create(another user's contact)", err, schedules.ErrUnknownContact)
	wantErr(t, "Create(another user's contact)", err, domain.ErrNotFound)
	_, err = e.svc.Update(ctx, e.alice, sc.ID, sc.Version, in)
	wantErr(t, "Update(another user's contact)", err, domain.ErrNotFound)

	_, err = e.svc.Get(ctx, e.alice, domain.ScheduleID{})
	wantErr(t, "Get(zero id)", err, domain.ErrInvalidID)
}

func TestUpdateReschedules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly")) // next 13:00
	e.clock.Advance(3 * time.Hour)             // 15:00: 13:00 and 14:00 are due, not fired yet

	// A rename keeps the fire state: the due occurrences still fire.
	in := e.input("renamed")
	got, err := e.svc.Update(ctx, e.alice, sc.ID, sc.Version, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Version != 2 || !got.NextRunAt.Equal(sc.NextRunAt) {
		t.Errorf("Update(rename) = %+v", got)
	}
	_, err = e.svc.Update(ctx, e.alice, sc.ID, sc.Version, in)
	wantErr(t, "Update(stale version)", err, domain.ErrConflict)

	// A new expression recomputes it from now.
	in.Cron = "30 * * * *"
	got, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, in)
	if err != nil || !got.NextRunAt.Equal(t0.Add(3*time.Hour+30*time.Minute)) {
		t.Errorf("Update(cron) = (%v, %v)", got.NextRunAt, err)
	}
	in.Timezone = "Asia/Kolkata" // +05:30: :30 UTC is :00 local
	got, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, in)
	if err != nil || !got.NextRunAt.Equal(t0.Add(4*time.Hour)) {
		t.Errorf("Update(timezone) = (%v, %v)", got.NextRunAt, err)
	}

	// Disabling cancels the open run; enabling clears the paused reason.
	open, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	in.Enabled = false
	got, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, in)
	if err != nil || got.Enabled || !got.NextRunAt.IsZero() {
		t.Fatalf("Update(disable) = (%+v, %v)", got, err)
	}
	if r, _ := e.store.GetRun(ctx, open.ID); r.Status != schedules.RunCancelled || r.Reason != schedules.ReasonDisabled {
		t.Errorf("open run after disabling = %+v", r)
	}
	if _, err := e.store.SetScheduleEnabled(ctx, sc.ID, schedules.Toggle{Enabled: false, PausedReason: schedules.ReasonWorkerRemoved, At: t0}); err != nil {
		t.Fatal(err)
	}
	got = e.schedule(sc.ID)
	in.Name = "still paused" // an edit of a paused schedule keeps its reason
	got, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, in)
	if err != nil || got.PausedReason != schedules.ReasonWorkerRemoved {
		t.Errorf("Update(paused) = (%+v, %v)", got, err)
	}
	in.Enabled = true
	got, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, in)
	if err != nil || !got.Enabled || got.PausedReason != "" || got.NextRunAt.IsZero() {
		t.Errorf("Update(enable) = (%+v, %v)", got, err)
	}

	_, err = e.svc.Update(ctx, e.alice, domain.NewScheduleID(), 1, in)
	wantErr(t, "Update(missing)", err, domain.ErrNotFound)
	bad := in
	bad.Cron = "nope"
	_, err = e.svc.Update(ctx, e.alice, sc.ID, got.Version, bad)
	wantErr(t, "Update(invalid)", err, domain.ErrInvalidArgument)
}

func TestSetEnabled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly"))
	open, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	off, err := e.svc.SetEnabled(ctx, e.alice, sc.ID, false)
	if err != nil || off.Enabled || !off.NextRunAt.IsZero() || off.Version != 2 || off.PausedReason != "" {
		t.Fatalf("SetEnabled(false) = (%+v, %v)", off, err)
	}
	if r, _ := e.store.GetRun(ctx, open.ID); r.Status != schedules.RunCancelled {
		t.Errorf("open run after pausing = %+v", r)
	}
	// Idempotent: no change, no new version.
	if again, err := e.svc.SetEnabled(ctx, e.alice, sc.ID, false); err != nil || again.Version != 2 {
		t.Errorf("SetEnabled(false) again = (v%d, %v)", again.Version, err)
	}
	if _, err := e.store.SetScheduleEnabled(ctx, sc.ID, schedules.Toggle{Enabled: false, PausedReason: schedules.ReasonOwnerDisabled, At: t0}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(90 * time.Minute) // 13:30
	on, err := e.svc.SetEnabled(ctx, e.alice, sc.ID, true)
	if err != nil || !on.Enabled || on.PausedReason != "" || !on.NextRunAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("SetEnabled(true) = (%+v, %v)", on, err)
	}
	e.clock.Advance(time.Hour) // 14:30: 14:00 is due
	if again, err := e.svc.SetEnabled(ctx, e.alice, sc.ID, true); err != nil || !again.NextRunAt.Equal(on.NextRunAt) {
		t.Errorf("SetEnabled(true) again moved the due run: (%v, %v)", again.NextRunAt, err)
	}
}

func TestDeleteKeepsJobs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("doomed"))
	j := e.addJob(sc, domain.JobSucceeded)
	if err := e.svc.Delete(ctx, e.alice, sc.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.store.GetJob(ctx, j.ID); err != nil || !got.ScheduleID.IsZero() {
		t.Errorf("job after the delete = (%v, %v)", got.ScheduleID, err)
	}
	wantErr(t, "Delete(again)", e.svc.Delete(ctx, e.alice, sc.ID), domain.ErrNotFound)
}

func TestRunNow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly"))
	e.clock.Advance(10 * time.Minute)
	r, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(10 * time.Minute)
	if r.Trigger != schedules.TriggerManual || r.Status != schedules.RunPending || !r.ScheduledFor.Equal(now) ||
		!r.Deadline.Equal(now.Add(time.Hour)) || r.Coalesced != 0 {
		t.Errorf("RunNow = %+v", r)
	}
	// A second request merges into the open run.
	again, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil || again.ID != r.ID || again.Coalesced != 1 {
		t.Errorf("RunNow(again) = (%+v, %v)", again, err)
	}

	// Skip overlap with an active job: FailedPrecondition.
	e.addJob(sc, domain.JobRunning)
	_, err = e.svc.RunNow(ctx, e.alice, sc.ID)
	wantErr(t, "RunNow(skip, active)", err, domain.ErrFailedPrecondition)
	// Queue overlap: the run waits.
	in := e.input("hourly")
	in.Overlap = schedules.OverlapQueue
	cur := e.schedule(sc.ID)
	if _, err := e.svc.Update(ctx, e.alice, sc.ID, cur.Version, in); err != nil {
		t.Fatal(err)
	}
	if q, err := e.svc.RunNow(ctx, e.alice, sc.ID); err != nil || q.ID != r.ID || q.Coalesced != 2 {
		t.Errorf("RunNow(queue, active) = (%+v, %v)", q, err)
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p, err := e.svc.Preview(" 30 2 * * * ", "Europe/Berlin", time.Time{}, 0)
	if err != nil || len(p.NextRuns) != schedules.DefaultPreview || p.Normalized != "30 2 * * *" ||
		!p.NextRuns[0].Equal(time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC)) {
		t.Errorf("Preview = (%+v, %v)", p, err)
	}
	after := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	if p, err := e.svc.Preview("30 2 * * *", "Europe/Berlin", after, 2); err != nil ||
		!p.NextRuns[0].Equal(time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)) || len(p.NextRuns) != 2 {
		t.Errorf("Preview(spring forward) = (%v, %v)", p.NextRuns, err)
	}
	for _, tt := range []struct {
		cron, tz string
		count    int
		want     []string
	}{
		{"@every 1h", "UTC", 5, []string{"cron"}},
		{"0 2 * * *", "Local", 5, []string{"timezone"}},
		{"0 2 * * *", "UTC", 21, []string{"count"}},
		{"0 2 * * *", "UTC", -1, []string{"count"}},
	} {
		_, err := e.svc.Preview(tt.cron, tt.tz, time.Time{}, tt.count)
		if got := fieldsOf(t, err); !slices.Equal(got, tt.want) {
			t.Errorf("Preview(%q, %q, %d) fields = %v, want %v", tt.cron, tt.tz, tt.count, got, tt.want)
		}
	}
}

func TestFireDueCoalescesDowntime(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly")) // next 13:00
	if n, err := e.svc.FireDue(ctx); err != nil || n != 0 {
		t.Fatalf("FireDue(nothing due) = (%d, %v)", n, err)
	}
	// The controller was down until 17:30: one run for 17:00, coalescing
	// 13:00 to 16:00.
	e.clock.Set(t0.Add(5*time.Hour + 30*time.Minute))
	if n, err := e.svc.FireDue(ctx); err != nil || n != 1 {
		t.Fatalf("FireDue = (%d, %v)", n, err)
	}
	rs := e.runs(sc.ID)
	if len(rs) != 1 || !rs[0].ScheduledFor.Equal(t0.Add(5*time.Hour)) || rs[0].Coalesced != 4 ||
		rs[0].Status != schedules.RunPending || rs[0].Trigger != schedules.TriggerCron ||
		!rs[0].Deadline.Equal(t0.Add(6*time.Hour)) || !rs[0].CreatedAt.Equal(e.clock.Now()) {
		t.Errorf("runs = %+v", rs)
	}
	got := e.schedule(sc.ID)
	if !got.NextRunAt.Equal(t0.Add(6*time.Hour)) || !got.LastFiredAt.Equal(e.clock.Now()) || got.Version != sc.Version {
		t.Errorf("fire state = next %v, last %v, v%d", got.NextRunAt, got.LastFiredAt, got.Version)
	}
	if n, _ := e.svc.FireDue(ctx); n != 0 {
		t.Errorf("FireDue(again) fired %d", n)
	}
	// The next occurrence merges into the still open run.
	e.clock.Set(t0.Add(6 * time.Hour))
	if n, err := e.svc.FireDue(ctx); err != nil || n != 1 {
		t.Fatalf("FireDue(next) = (%d, %v)", n, err)
	}
	rs = e.runs(sc.ID)
	if len(rs) != 1 || rs[0].Coalesced != 5 || !rs[0].Deadline.Equal(t0.Add(7*time.Hour)) {
		t.Errorf("merged run = %+v", rs)
	}
}

// TestFireDueOverlapMatrix fires each overlap policy against an active job,
// an open (waiting) run and nothing.
func TestFireDueOverlapMatrix(t *testing.T) {
	t.Parallel()
	type outcome struct {
		runs      int
		newStatus schedules.RunStatus
		coalesced int
	}
	tests := []struct {
		overlap schedules.Overlap
		state   string
		want    outcome
	}{
		{schedules.OverlapSkip, "active", outcome{1, schedules.RunMissed, 0}},
		{schedules.OverlapSkip, "open", outcome{1, schedules.RunPending, 1}},
		{schedules.OverlapSkip, "none", outcome{1, schedules.RunPending, 0}},
		{schedules.OverlapQueue, "active", outcome{1, schedules.RunPending, 0}},
		{schedules.OverlapQueue, "open", outcome{1, schedules.RunPending, 1}},
		{schedules.OverlapQueue, "none", outcome{1, schedules.RunPending, 0}},
		{schedules.OverlapAllow, "active", outcome{1, schedules.RunPending, 0}},
		{schedules.OverlapAllow, "open", outcome{1, schedules.RunPending, 1}},
		{schedules.OverlapAllow, "none", outcome{1, schedules.RunPending, 0}},
	}
	for _, tt := range tests {
		t.Run(string(tt.overlap)+"/"+tt.state, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			ctx := context.Background()
			in := e.input("s")
			in.Overlap = tt.overlap
			sc := e.create(e.alice, in)
			var pre []domain.ScheduleRunID
			switch tt.state {
			case "active":
				e.addJob(sc, domain.JobRunning)
			case "open":
				r, err := e.svc.RunNow(ctx, e.alice, sc.ID)
				if err != nil {
					t.Fatal(err)
				}
				pre = append(pre, r.ID)
			}
			e.clock.Set(sc.NextRunAt)
			if n, err := e.svc.FireDue(ctx); err != nil || n != 1 {
				t.Fatalf("FireDue = (%d, %v)", n, err)
			}
			rs := e.runs(sc.ID)
			if len(rs) != tt.want.runs {
				t.Fatalf("runs = %+v, want %d", rs, tt.want.runs)
			}
			r := rs[0]
			if r.Status != tt.want.newStatus || r.Coalesced != tt.want.coalesced ||
				(tt.state == "open") != slices.Contains(pre, r.ID) {
				t.Errorf("run = %+v, want %+v", r, tt.want)
			}
			if r.Status == schedules.RunMissed && r.Reason != schedules.ReasonOverlap {
				t.Errorf("missed run reason = %q", r.Reason)
			}
		})
	}
}

// TestFireDueConcurrent: two services (two controllers) firing at once
// store one fire and one run.
func TestFireDueConcurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly"))
	other, err := schedules.New(schedules.Deps{
		Repo: e.store, Remotes: e.remotes, Contacts: e.contacts, Workers: storeWorkers{e.store},
		Waker: e.waker, Clock: e.clock, IDs: domainIDs{},
	}, schedules.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Set(sc.NextRunAt)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := range 8 {
		svc := e.svc
		if i%2 == 1 {
			svc = other
		}
		wg.Go(func() {
			n, err := svc.FireDue(ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if rs := e.runs(sc.ID); total != 1 || len(rs) != 1 {
		t.Errorf("fires = %d, runs = %d, want 1 and 1", total, len(rs))
	}
}

// domainIDs mints UUIDv7s (a second generator that cannot collide with
// the first).
type domainIDs struct{}

func (domainIDs) NewScheduleID() domain.ScheduleID       { return domain.NewScheduleID() }
func (domainIDs) NewScheduleRunID() domain.ScheduleRunID { return domain.NewScheduleRunID() }

func TestFireDuePausesAnUnparsableSchedule(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("hourly"))
	// The host lost the schedule's zone (stored directly: the service
	// would reject it).
	broken := e.schedule(sc.ID)
	broken.Timezone = "Gone/Zone"
	if _, err := e.store.UpdateSchedule(ctx, broken, false); err != nil {
		t.Fatal(err)
	}
	e.clock.Set(sc.NextRunAt)
	if n, err := e.svc.FireDue(ctx); err != nil || n != 0 {
		t.Fatalf("FireDue = (%d, %v)", n, err)
	}
	if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonInvalidDefinition {
		t.Errorf("schedule = %+v, want paused", got)
	}
}

func TestRunsPaging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := e.input("allow")
	in.Overlap = schedules.OverlapAllow
	sc := e.create(e.alice, in)
	var ids []domain.ScheduleRunID
	for range 5 {
		r, err := e.svc.RunNow(ctx, e.alice, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.EndRun(ctx, r, schedules.RunMissed, schedules.ReasonWorkerOffline, "", false); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	slices.Reverse(ids)
	p, err := e.svc.Runs(ctx, e.alice, sc.ID, domain.ScheduleRunID{}, 2)
	if err != nil || len(p.Runs) != 2 || p.Runs[0].ID != ids[0] || p.Next != ids[1] {
		t.Fatalf("page 1 = (%+v, %v)", p, err)
	}
	p, err = e.svc.Runs(ctx, e.alice, sc.ID, p.Next, 3)
	if err != nil || len(p.Runs) != 3 || p.Runs[2].ID != ids[4] || !p.Next.IsZero() {
		t.Errorf("page 2 = (%+v, %v)", p, err)
	}
	_, err = e.svc.Runs(ctx, e.alice, sc.ID, domain.ScheduleRunID{}, schedules.MaxPageSize+1)
	wantErr(t, "Runs(limit too big)", err, domain.ErrInvalidArgument)
}

func TestDashboardParts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(e.alice, e.input("a")) // 13:00
	in := e.input("b")
	in.Cron = "30 12 * * *"
	b := e.create(e.alice, in) // 12:30
	e.create(e.bob, e.input("bobs"))
	up, err := e.svc.Upcoming(ctx, e.alice, 0)
	if err != nil || len(up) != 2 || up[0].ScheduleID != b.ID || up[1].ScheduleID != a.ID || up[1].Name != "a" ||
		up[1].Target != a.Target || !up[1].At.Equal(a.NextRunAt) {
		t.Errorf("Upcoming = (%+v, %v)", up, err)
	}
	if up, _ := e.svc.Upcoming(ctx, e.alice, 1); len(up) != 1 {
		t.Errorf("Upcoming(1) = %v", up)
	}
	// One missed and one failed run of a, one launched.
	r1, _ := e.svc.RunNow(ctx, e.alice, a.ID)
	if _, err := e.svc.EndRun(ctx, r1, schedules.RunMissed, schedules.ReasonWorkerOffline, "", false); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	r2, _ := e.svc.RunNow(ctx, e.alice, a.ID)
	if _, err := e.svc.EndRun(ctx, r2, schedules.RunFailed, schedules.ReasonNoWorkerWithLabel, "no worker", false); err != nil {
		t.Fatal(err)
	}
	probs, err := e.svc.RecentProblems(ctx, e.alice, 0)
	if err != nil || len(probs) != 2 || probs[0].ID != r2.ID || probs[1].ID != r1.ID {
		t.Errorf("RecentProblems = (%+v, %v)", probs, err)
	}
	if n, err := e.svc.CountMissed(ctx, e.alice, t0, t0.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("CountMissed = (%d, %v)", n, err)
	}
	if n, _ := e.svc.CountMissed(ctx, e.alice, t0.Add(time.Second), time.Time{}); n != 0 {
		t.Errorf("CountMissed(after the run) = %d", n)
	}
	if n, _ := e.svc.CountMissed(ctx, e.bob, time.Time{}, time.Time{}); n != 0 {
		t.Errorf("CountMissed(bob) = %d", n)
	}
	names, err := e.svc.Names(ctx, []domain.ScheduleID{a.ID, domain.NewScheduleID()})
	if err != nil || len(names) != 1 || names[a.ID].Name != "a" {
		t.Errorf("Names = (%v, %v)", names, err)
	}
}

func TestLaunchSteps(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	sc := e.create(e.alice, e.input("s"))
	r, err := e.svc.RunNow(ctx, e.alice, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := domain.NewJobID()
	_, err = e.svc.MarkLaunching(ctx, r, domain.JobID{}, e.worker)
	wantErr(t, "MarkLaunching(no job)", err, domain.ErrInvalidID)
	l, err := e.svc.MarkLaunching(ctx, r, job, e.worker)
	if err != nil || l.Status != schedules.RunLaunching || l.JobID != job || l.WorkerID != e.worker {
		t.Fatalf("MarkLaunching = (%+v, %v)", l, err)
	}
	_, err = e.svc.MarkLaunching(ctx, r, domain.NewJobID(), e.worker)
	wantErr(t, "MarkLaunching(twice)", err, domain.ErrConflict)
	if active, _ := e.svc.HasActiveRun(ctx, sc.ID); !active {
		t.Error("a launching run is not active")
	}
	// Back to pending (the worker disappeared): the reserved job is kept.
	p, err := e.svc.EndRun(ctx, l, schedules.RunPending, "ignored", "ignored", true)
	if err != nil || p.Status != schedules.RunPending || p.JobID != job || p.Reason != "" || !e.schedule(sc.ID).Enabled {
		t.Errorf("EndRun(pending) = (%+v, %v)", p, err)
	}
	open, err := e.svc.OpenRuns(ctx, -1)
	if err != nil || len(open) != 1 || open[0].ID != r.ID {
		t.Errorf("OpenRuns = (%v, %v)", open, err)
	}
	loaded, err := e.svc.Load(ctx, []domain.ScheduleID{sc.ID, domain.NewScheduleID()})
	if err != nil || len(loaded) != 1 || loaded[sc.ID].Name != "s" {
		t.Errorf("Load = (%v, %v)", loaded, err)
	}
	l, _ = e.svc.MarkLaunching(ctx, p, job, e.worker)
	if done, err := e.svc.MarkLaunched(ctx, l); err != nil || done.Status != schedules.RunLaunched {
		t.Errorf("MarkLaunched = (%+v, %v)", done, err)
	}
	_, err = e.svc.EndRun(ctx, l, schedules.RunFailed, schedules.ReasonWorkerRemoved, "", true)
	wantErr(t, "EndRun(stale)", err, domain.ErrConflict)
	_, err = e.svc.EndRun(ctx, schedules.Run{ID: r.ID, Status: schedules.RunLaunched}, schedules.RunFailed, "", "", false)
	wantErr(t, "EndRun(terminal)", err, domain.ErrFailedPrecondition)
	_, err = e.svc.EndRun(ctx, p, schedules.RunLaunched, "", "", false)
	wantErr(t, "EndRun(launched)", err, domain.ErrInvalidArgument)

	// Ending with pause disables the schedule.
	r2, _ := e.svc.RunNow(ctx, e.alice, sc.ID)
	if _, err := e.svc.EndRun(ctx, r2, schedules.RunFailed, schedules.ReasonWorkerRemoved, "removed", true); err != nil {
		t.Fatal(err)
	}
	if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonWorkerRemoved || !got.NextRunAt.IsZero() {
		t.Errorf("schedule after an auto-pause = %+v", got)
	}
}
