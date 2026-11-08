package jobs_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeActivity is a jobs.ActivityRepository returning canned rows and
// remembering the queries.
type fakeActivity struct {
	mu        sync.Mutex
	rows      []jobs.ActivityRow
	counts    map[domain.JobStatus]int64
	err       error
	queries   []jobs.ActivityQuery
	countArgs []domain.UserID
}

func (f *fakeActivity) JobActivity(_ context.Context, q jobs.ActivityQuery) ([]jobs.ActivityRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.rows, f.err
}

func (f *fakeActivity) CountUnfinished(_ context.Context, owner domain.UserID) (map[domain.JobStatus]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countArgs = append(f.countArgs, owner)
	return maps.Clone(f.counts), f.err
}

// newActivityService returns a jobs service over the env's store whose
// activity repository is repo.
func newActivityService(t *testing.T, e *env, repo jobs.ActivityRepository) *jobs.Service {
	t.Helper()
	svc, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: broker.New(), Notifier: e.rec, Waker: e.rec, Remotes: e.remotes, Activity: repo,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(y int, m time.Month, d, h, minute int) time.Time {
	return time.Date(y, m, d, h, minute, 0, 0, time.UTC)
}

func starts(bs []jobs.ActivityBucket) []time.Time {
	out := make([]time.Time, len(bs))
	for i, b := range bs {
		out[i] = b.Start
	}
	return out
}

func TestActivityFillsEveryBucket(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	svc := newActivityService(t, e, &fakeActivity{})
	berlin, kolkata, saoPaulo := zone(t, "Europe/Berlin"), zone(t, "Asia/Kolkata"), zone(t, "America/Sao_Paulo")
	tests := []struct {
		name string
		f    jobs.ActivityFilter
		want []time.Time
	}{
		{
			"UTC hours, partial first and last",
			jobs.ActivityFilter{From: at(2026, 11, 4, 9, 17), Until: at(2026, 11, 4, 12, 5), Unit: jobs.BucketHour, Location: time.UTC},
			[]time.Time{at(2026, 11, 4, 9, 0), at(2026, 11, 4, 10, 0), at(2026, 11, 4, 11, 0), at(2026, 11, 4, 12, 0)},
		},
		{
			"Berlin hours across the fall-back: the repeated hour twice",
			jobs.ActivityFilter{From: at(2026, 10, 24, 23, 0), Until: at(2026, 10, 25, 3, 0), Unit: jobs.BucketHour, Location: berlin},
			[]time.Time{at(2026, 10, 24, 23, 0), at(2026, 10, 25, 0, 0), at(2026, 10, 25, 1, 0), at(2026, 10, 25, 2, 0)},
		},
		{
			"Berlin hours across the spring-forward",
			jobs.ActivityFilter{From: at(2026, 3, 29, 0, 0), Until: at(2026, 3, 29, 3, 0), Unit: jobs.BucketHour, Location: berlin},
			[]time.Time{at(2026, 3, 29, 0, 0), at(2026, 3, 29, 1, 0), at(2026, 3, 29, 2, 0)},
		},
		{
			"Kolkata hours start at half past",
			jobs.ActivityFilter{From: at(2026, 11, 4, 0, 0), Until: at(2026, 11, 4, 2, 0), Unit: jobs.BucketHour, Location: kolkata},
			[]time.Time{at(2026, 11, 3, 23, 30), at(2026, 11, 4, 0, 30), at(2026, 11, 4, 1, 30)},
		},
		{
			"Berlin days across the fall-back (a 25-hour day)",
			jobs.ActivityFilter{From: at(2026, 10, 24, 22, 0), Until: at(2026, 10, 27, 23, 0), Unit: jobs.BucketDay, Location: berlin},
			[]time.Time{at(2026, 10, 24, 22, 0), at(2026, 10, 25, 23, 0), at(2026, 10, 26, 23, 0)},
		},
		{
			"Berlin days across the spring-forward (a 23-hour day)",
			jobs.ActivityFilter{From: at(2026, 3, 28, 12, 0), Until: at(2026, 3, 30, 12, 0), Unit: jobs.BucketDay, Location: berlin},
			[]time.Time{at(2026, 3, 27, 23, 0), at(2026, 3, 28, 23, 0), at(2026, 3, 29, 22, 0)},
		},
		{
			"Sao Paulo day without a midnight",
			jobs.ActivityFilter{From: at(2018, 11, 3, 12, 0), Until: at(2018, 11, 5, 12, 0), Unit: jobs.BucketDay, Location: saoPaulo},
			[]time.Time{at(2018, 11, 3, 3, 0), at(2018, 11, 4, 3, 0), at(2018, 11, 5, 2, 0)},
		},
		{
			"Lord Howe hours across a 30-minute shift",
			jobs.ActivityFilter{From: at(2026, 4, 4, 13, 0), Until: at(2026, 4, 4, 16, 0), Unit: jobs.BucketHour, Location: zone(t, "Australia/Lord_Howe")},
			// +11 until 15:00 UTC (02:00 local), then +10:30.
			[]time.Time{at(2026, 4, 4, 13, 0), at(2026, 4, 4, 14, 0), at(2026, 4, 4, 14, 30), at(2026, 4, 4, 15, 30)},
		},
	}
	for _, tt := range tests {
		got, err := svc.Activity(context.Background(), e.owner, tt.f)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if g := starts(got); !slices.EqualFunc(g, tt.want, time.Time.Equal) {
			t.Errorf("%s: buckets\n got %v\nwant %v", tt.name, g, tt.want)
		}
		for _, b := range got {
			if b.Succeeded+b.Failed+b.Interrupted+b.Cancelled+b.Bytes+b.Files != 0 {
				t.Errorf("%s: bucket %v is not empty: %+v", tt.name, b.Start, b)
			}
		}
	}
}

func TestActivityMergesRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	repo := &fakeActivity{rows: []jobs.ActivityRow{
		{Bucket: at(2026, 11, 4, 10, 0), Status: domain.JobSucceeded, Jobs: 3, Bytes: 300, Files: 30},
		{Bucket: at(2026, 11, 4, 10, 0), Status: domain.JobFailed, Jobs: 1, Bytes: 5, Files: 1},
		{Bucket: at(2026, 11, 4, 11, 0), Status: domain.JobInterrupted, Jobs: 2},
		{Bucket: at(2026, 11, 4, 11, 0), Status: domain.JobCancelled, Jobs: 4, Bytes: 1},
		{Bucket: at(2026, 11, 4, 11, 0), Status: domain.JobRunning, Jobs: 9, Bytes: 9}, // ignored
		{Bucket: at(2026, 11, 4, 10, 30), Status: domain.JobSucceeded, Jobs: 1},        // unknown boundary: kept
	}}
	svc := newActivityService(t, e, repo)
	got, err := svc.Activity(context.Background(), e.owner, jobs.ActivityFilter{
		From: at(2026, 11, 4, 10, 0), Until: at(2026, 11, 4, 12, 0), Unit: jobs.BucketHour, Location: time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []jobs.ActivityBucket{
		{Start: at(2026, 11, 4, 10, 0), Succeeded: 3, Failed: 1, Bytes: 305, Files: 31},
		{Start: at(2026, 11, 4, 10, 30), Succeeded: 1},
		{Start: at(2026, 11, 4, 11, 0), Interrupted: 2, Cancelled: 4, Bytes: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %+v, want %+v", got, want)
	}
	for i := range want {
		if g, w := got[i], want[i]; !g.Start.Equal(w.Start) || g.Succeeded != w.Succeeded || g.Failed != w.Failed ||
			g.Interrupted != w.Interrupted || g.Cancelled != w.Cancelled || g.Bytes != w.Bytes || g.Files != w.Files {
			t.Errorf("bucket %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestActivityScopesByActor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.addUser(t, domain.RoleAdmin)
	f := jobs.ActivityFilter{From: at(2026, 11, 4, 10, 0), Until: at(2026, 11, 4, 11, 0), Unit: jobs.BucketHour, Location: time.UTC}
	tests := []struct {
		name     string
		actor    domain.Actor
		allUsers bool
		want     domain.UserID
		queried  bool
	}{
		{"user", e.owner, false, e.owner.UserID, true},
		{"user asking for all users", e.owner, true, e.owner.UserID, true},
		{"admin", admin, false, admin.UserID, true},
		{"admin with all users", admin, true, domain.UserID{}, true},
		{"no user", domain.Actor{Role: domain.RoleUser}, true, domain.UserID{}, false},
	}
	for _, tt := range tests {
		repo := &fakeActivity{counts: map[domain.JobStatus]int64{domain.JobQueued: 2}}
		svc := newActivityService(t, e, repo)
		f.AllUsers = tt.allUsers
		bs, err := svc.Activity(context.Background(), tt.actor, f)
		if err != nil || len(bs) != 1 {
			t.Errorf("%s: Activity = (%v, %v)", tt.name, bs, err)
		}
		counts, err := svc.CountUnfinished(context.Background(), tt.actor, tt.allUsers)
		if err != nil {
			t.Errorf("%s: CountUnfinished: %v", tt.name, err)
		}
		if !tt.queried {
			if len(repo.queries)+len(repo.countArgs) != 0 || len(counts) != 0 {
				t.Errorf("%s: the store was queried (%v, %v) or counts returned %v", tt.name, repo.queries, repo.countArgs, counts)
			}
			continue
		}
		if len(repo.queries) != 1 || repo.queries[0].OwnerID != tt.want || len(repo.countArgs) != 1 || repo.countArgs[0] != tt.want {
			t.Errorf("%s: queried owners %v / %v, want %v", tt.name, repo.queries, repo.countArgs, tt.want)
		}
		if counts[domain.JobQueued] != 2 {
			t.Errorf("%s: counts = %v", tt.name, counts)
		}
	}
}

func TestActivityRejects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	svc := newActivityService(t, e, &fakeActivity{})
	from := at(2026, 11, 4, 0, 0)
	tests := []struct {
		name  string
		f     jobs.ActivityFilter
		field string
	}{
		{"unknown unit", jobs.ActivityFilter{From: from, Until: from.Add(time.Hour), Unit: "week", Location: time.UTC}, "unit"},
		{"no zone", jobs.ActivityFilter{From: from, Until: from.Add(time.Hour), Unit: jobs.BucketHour}, "timezone"},
		{"local zone", jobs.ActivityFilter{From: from, Until: from.Add(time.Hour), Unit: jobs.BucketHour, Location: time.Local}, "timezone"},
		{"empty range", jobs.ActivityFilter{From: from, Until: from, Unit: jobs.BucketHour, Location: time.UTC}, "range"},
		{"reversed range", jobs.ActivityFilter{From: from, Until: from.Add(-time.Hour), Unit: jobs.BucketDay, Location: time.UTC}, "range"},
		{"zero from", jobs.ActivityFilter{Until: from, Unit: jobs.BucketDay, Location: time.UTC}, "range"},
		{"too long", jobs.ActivityFilter{From: from, Until: from.Add(jobs.MaxActivityRange + time.Hour), Unit: jobs.BucketDay, Location: time.UTC}, "range"},
		{"too many hours", jobs.ActivityFilter{From: from, Until: from.Add(jobs.MaxHourlyActivityRange + time.Hour), Unit: jobs.BucketHour, Location: time.UTC}, "range"},
	}
	for _, tt := range tests {
		_, err := svc.Activity(context.Background(), e.owner, tt.f)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || len(verr.Fields) == 0 || verr.Fields[0].Field != tt.field {
			t.Errorf("%s: %v, want a validation error on %s", tt.name, err, tt.field)
		}
	}
	// Without the repository the use cases fail; store errors pass through.
	ctx := context.Background()
	ok := jobs.ActivityFilter{From: from, Until: from.Add(time.Hour), Unit: jobs.BucketHour, Location: time.UTC}
	if _, err := e.svc.Activity(ctx, e.owner, ok); err == nil {
		t.Error("Activity without an activity repository succeeded")
	}
	if _, err := e.svc.CountUnfinished(ctx, e.owner, false); err == nil {
		t.Error("CountUnfinished without an activity repository succeeded")
	}
	boom := errors.New("boom")
	failing := newActivityService(t, e, &fakeActivity{err: boom})
	if _, err := failing.Activity(ctx, e.owner, ok); !errors.Is(err, boom) {
		t.Errorf("Activity store error = %v", err)
	}
	if _, err := failing.CountUnfinished(ctx, e.owner, false); !errors.Is(err, boom) {
		t.Errorf("CountUnfinished store error = %v", err)
	}
}

// TestActivityWithMemstore checks that the service's buckets and the
// store's (Postgres semantics) agree across DST changes: every row lands in
// a generated bucket, none is added.
func TestActivityWithMemstore(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	svc := newActivityService(t, e, e.store)
	ctx := context.Background()
	finish := func(when time.Time, st domain.JobStatus, bytes int64) {
		t.Helper()
		j := e.create(t)
		j.Status, j.FinishedAt, j.Stats = st, when, jobs.Stats{Bytes: bytes, Files: 1}
		if _, err := e.store.UpdateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	finish(at(2026, 10, 24, 23, 30), domain.JobSucceeded, 1) // Oct 25, 01:30 CEST
	finish(at(2026, 10, 25, 0, 30), domain.JobFailed, 2)     // 02:30 CEST
	finish(at(2026, 10, 25, 1, 30), domain.JobSucceeded, 4)  // 02:30 CET
	finish(at(2026, 10, 25, 22, 59), domain.JobCancelled, 8) // 23:59 CET, Oct 25
	finish(at(2026, 10, 25, 23, 0), domain.JobSucceeded, 16) // 00:00 CET, Oct 26
	other := e.addUser(t, domain.RoleUser)
	j := e.createAs(t, other, jobs.CreateParams{})
	j.Status, j.FinishedAt = domain.JobSucceeded, at(2026, 10, 25, 0, 45)
	if _, err := e.store.UpdateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	berlin := zone(t, "Europe/Berlin")

	days, err := svc.Activity(ctx, e.owner, jobs.ActivityFilter{From: at(2026, 10, 24, 22, 0), Until: at(2026, 10, 26, 23, 0), Unit: jobs.BucketDay, Location: berlin})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].Succeeded != 2 || days[0].Failed != 1 || days[0].Cancelled != 1 || days[0].Bytes != 15 ||
		days[1].Succeeded != 1 || days[1].Bytes != 16 {
		t.Errorf("days = %+v", days)
	}

	hours, err := svc.Activity(ctx, e.owner, jobs.ActivityFilter{From: at(2026, 10, 24, 23, 0), Until: at(2026, 10, 25, 2, 0), Unit: jobs.BucketHour, Location: berlin})
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 3 || hours[0].Succeeded != 1 || hours[1].Failed != 1 || hours[2].Succeeded != 1 {
		t.Errorf("hours = %+v", hours)
	}

	admin := e.addUser(t, domain.RoleAdmin)
	all, err := svc.Activity(ctx, admin, jobs.ActivityFilter{From: at(2026, 10, 25, 0, 0), Until: at(2026, 10, 25, 1, 0), Unit: jobs.BucketHour, Location: berlin, AllUsers: true})
	if err != nil || len(all) != 1 || all[0].Succeeded != 1 || all[0].Failed != 1 {
		t.Errorf("all users = (%+v, %v)", all, err)
	}

	e.create(t) // queued
	counts, err := svc.CountUnfinished(ctx, e.owner, false)
	if err != nil || !maps.Equal(counts, map[domain.JobStatus]int64{domain.JobQueued: 1}) {
		t.Errorf("CountUnfinished = (%v, %v)", counts, err)
	}
}
