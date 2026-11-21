package storetest

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of jobs.ActivityRepository. The expected
// buckets are what Postgres 17 computes with date_trunc(unit, finished_at,
// tz): an hour bucket keeps the job's UTC offset (both passes of a repeated
// hour get their own bucket; +05:30 buckets at half past), a day bucket is
// local midnight resolved again (a skipped midnight resolves to the
// transition instant).

// requireActivity skips the test while the activity port is a stub.
func requireActivity(t *testing.T, s Store) {
	t.Helper()
	_, err := s.UnfinishedJobs(context.Background(), domain.UserID{})
	skipIfUnsupported(t, "UnfinishedJobs", "track C (dashboard)", err)
}

func loadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(y int, m time.Month, d, h, minute int) time.Time {
	return time.Date(y, m, d, h, minute, 0, 0, time.UTC)
}

func testActivityBuckets(t *testing.T, s Store) {
	ctx := context.Background()
	requireActivity(t, s)
	owner, other := ownerID(t), userID(t, 2)
	ensureUser(t, s, owner)
	ensureUser(t, s, other)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	n := 0
	finish := func(at time.Time, st domain.JobStatus, bytes int64, by domain.UserID) {
		t.Helper()
		n++
		j := newJob(t, n, w.ID)
		j.OwnerID = by
		mustCreateJob(t, s, j)
		j.Status, j.FinishedAt, j.Stats = st, at, jobs.Stats{Bytes: bytes, Files: 1}
		if _, err := s.UpdateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	// Europe/Berlin falls back on 2026-10-25 at 01:00 UTC (03:00 CEST ->
	// 02:00 CET).
	finish(utc(2026, 10, 24, 23, 59), domain.JobSucceeded, 1, owner)  // 01:59 CEST
	finish(utc(2026, 10, 25, 0, 10), domain.JobSucceeded, 10, owner)  // 02:10 CEST
	finish(utc(2026, 10, 25, 0, 50), domain.JobFailed, 20, owner)     // 02:50 CEST
	finish(utc(2026, 10, 25, 1, 10), domain.JobSucceeded, 30, owner)  // 02:10 CET
	finish(utc(2026, 10, 25, 1, 40), domain.JobSucceeded, 40, owner)  // 02:40 CET
	finish(utc(2026, 10, 25, 2, 5), domain.JobInterrupted, 50, owner) // 03:05 CET
	finish(utc(2026, 10, 25, 22, 30), domain.JobSucceeded, 60, owner) // 23:30 CET, still the 25th
	finish(utc(2026, 10, 25, 23, 30), domain.JobSucceeded, 70, owner) // 00:30 CET on the 26th
	finish(utc(2026, 10, 25, 0, 20), domain.JobSucceeded, 1000, other)
	// Asia/Kolkata is +05:30.
	finish(utc(2026, 11, 4, 4, 45), domain.JobCancelled, 80, owner)  // 10:15 IST
	finish(utc(2026, 11, 4, 20, 0), domain.JobSucceeded, 90, owner)  // 01:30 IST on the 5th
	finish(utc(2018, 11, 4, 15, 0), domain.JobSucceeded, 100, owner) // Sao Paulo skipped midnight that day
	n++
	mustCreateJob(t, s, newJob(t, n, w.ID)) // queued: never counted

	// Every job transferred one file, so a row's files equal its jobs.
	type row struct {
		bucket      time.Time
		status      domain.JobStatus
		jobs, bytes int64
	}
	berlin, kolkata, saoPaulo := loadZone(t, "Europe/Berlin"), loadZone(t, "Asia/Kolkata"), loadZone(t, "America/Sao_Paulo")
	tests := []struct {
		name string
		q    jobs.ActivityQuery
		want []row
	}{
		{"Berlin hours across the fall-back", jobs.ActivityQuery{From: utc(2026, 10, 24, 23, 0), Until: utc(2026, 10, 25, 3, 0), Unit: jobs.BucketHour, Location: berlin},
			[]row{
				{utc(2026, 10, 24, 23, 0), domain.JobSucceeded, 1, 1},
				{utc(2026, 10, 25, 0, 0), domain.JobFailed, 1, 20},
				{utc(2026, 10, 25, 0, 0), domain.JobSucceeded, 2, 1010},
				{utc(2026, 10, 25, 1, 0), domain.JobSucceeded, 2, 70},
				{utc(2026, 10, 25, 2, 0), domain.JobInterrupted, 1, 50},
			}},
		{"one owner", jobs.ActivityQuery{From: utc(2026, 10, 25, 0, 0), Until: utc(2026, 10, 25, 1, 0), Unit: jobs.BucketHour, Location: berlin, OwnerID: owner},
			[]row{
				{utc(2026, 10, 25, 0, 0), domain.JobFailed, 1, 20},
				{utc(2026, 10, 25, 0, 0), domain.JobSucceeded, 1, 10},
			}},
		{"from inclusive, until exclusive", jobs.ActivityQuery{From: utc(2026, 10, 25, 0, 10), Until: utc(2026, 10, 25, 0, 50), Unit: jobs.BucketHour, Location: time.UTC},
			[]row{{utc(2026, 10, 25, 0, 0), domain.JobSucceeded, 2, 1010}}},
		{"Berlin days across the fall-back (a 25-hour day)", jobs.ActivityQuery{From: utc(2026, 10, 24, 22, 0), Until: utc(2026, 10, 26, 23, 0), Unit: jobs.BucketDay, Location: berlin, OwnerID: owner},
			[]row{
				{utc(2026, 10, 24, 22, 0), domain.JobFailed, 1, 20},
				{utc(2026, 10, 24, 22, 0), domain.JobInterrupted, 1, 50},
				{utc(2026, 10, 24, 22, 0), domain.JobSucceeded, 5, 141},
				{utc(2026, 10, 25, 23, 0), domain.JobSucceeded, 1, 70},
			}},
		{"Kolkata hours (+05:30)", jobs.ActivityQuery{From: utc(2026, 11, 4, 0, 0), Until: utc(2026, 11, 5, 0, 0), Unit: jobs.BucketHour, Location: kolkata},
			[]row{
				{utc(2026, 11, 4, 4, 30), domain.JobCancelled, 1, 80},
				{utc(2026, 11, 4, 19, 30), domain.JobSucceeded, 1, 90},
			}},
		{"Kolkata days", jobs.ActivityQuery{From: utc(2026, 11, 4, 0, 0), Until: utc(2026, 11, 5, 0, 0), Unit: jobs.BucketDay, Location: kolkata},
			[]row{
				{utc(2026, 11, 3, 18, 30), domain.JobCancelled, 1, 80},
				{utc(2026, 11, 4, 18, 30), domain.JobSucceeded, 1, 90},
			}},
		{"Sao Paulo day without a midnight", jobs.ActivityQuery{From: utc(2018, 11, 1, 0, 0), Until: utc(2018, 11, 10, 0, 0), Unit: jobs.BucketDay, Location: saoPaulo},
			[]row{{utc(2018, 11, 4, 3, 0), domain.JobSucceeded, 1, 100}}},
		{"empty range", jobs.ActivityQuery{From: utc(2020, 1, 1, 0, 0), Until: utc(2020, 1, 2, 0, 0), Unit: jobs.BucketDay, Location: berlin}, nil},
	}
	for _, tt := range tests {
		got, err := s.JobActivity(ctx, tt.q)
		if err != nil {
			t.Errorf("%s: JobActivity: %v", tt.name, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("%s: %d rows, want %d: %+v", tt.name, len(got), len(tt.want), got)
			continue
		}
		for i, w := range tt.want {
			g := got[i]
			if !g.Bucket.Equal(w.bucket) || g.Status != w.status || g.Jobs != w.jobs || g.Bytes != w.bytes || g.Files != w.jobs {
				t.Errorf("%s: row %d = {%v %s jobs %d bytes %d files %d}, want {%v %s jobs %d bytes %d}",
					tt.name, i, g.Bucket.UTC(), g.Status, g.Jobs, g.Bytes, g.Files, w.bucket, w.status, w.jobs, w.bytes)
			}
		}
	}
}

func testActivityUnfinished(t *testing.T, s Store) {
	ctx := context.Background()
	requireActivity(t, s)
	owner, other := ownerID(t), userID(t, 2)
	ensureUser(t, s, owner)
	ensureUser(t, s, other)
	w1, w2 := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	jobsIn := []struct {
		st     domain.JobStatus
		by     domain.UserID
		w      domain.WorkerID
		kind   string
		queued time.Time
	}{
		{domain.JobQueued, owner, w1.ID, "rclone_transfer", t0().Add(2 * time.Minute)},
		{domain.JobQueued, owner, w1.ID, "rclone_transfer", t0().Add(time.Minute)},
		{domain.JobQueued, owner, w1.ID, "restic_backup", t0()},
		{domain.JobQueued, owner, w2.ID, "rclone_transfer", t0()},
		{domain.JobDispatched, owner, w1.ID, "rclone_transfer", t0()},
		{domain.JobRunning, owner, w1.ID, "rclone_transfer", t0().Add(-time.Hour)},
		{domain.JobSucceeded, owner, w1.ID, "rclone_transfer", t0()},
		{domain.JobFailed, owner, w1.ID, "rclone_transfer", t0()},
		{domain.JobQueued, other, w1.ID, "rclone_transfer", t0().Add(-time.Minute)},
	}
	for i, tt := range jobsIn {
		j := newJob(t, i+1, tt.w)
		j.OwnerID, j.Kind, j.QueuedAt = tt.by, tt.kind, tt.queued
		mustCreateJob(t, s, j)
		if tt.st != domain.JobQueued {
			j.Status = tt.st
			if tt.st.Terminal() {
				j.FinishedAt = t0()
			}
			if _, err := s.UpdateJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
	}
	type key struct {
		st   domain.JobStatus
		kind string
		w    domain.WorkerID
	}
	type val struct {
		jobs   int64
		oldest time.Time
	}
	check := func(name string, owner domain.UserID, want map[key]val) {
		t.Helper()
		rows, err := s.UnfinishedJobs(ctx, owner)
		if err != nil {
			t.Fatalf("%s: UnfinishedJobs: %v", name, err)
		}
		got := map[key]val{}
		for _, r := range rows {
			k := key{r.Status, r.Kind, r.WorkerID}
			if _, dup := got[k]; dup {
				t.Errorf("%s: group %+v twice", name, k)
			}
			got[k] = val{r.Jobs, r.OldestQueuedAt.UTC()}
		}
		if !maps.EqualFunc(got, want, func(a, b val) bool { return a.jobs == b.jobs && a.oldest.Equal(b.oldest) }) {
			t.Errorf("%s: UnfinishedJobs = %v, want %v", name, got, want)
		}
	}
	mine := map[key]val{
		{domain.JobQueued, "rclone_transfer", w1.ID}:     {2, t0().Add(time.Minute)},
		{domain.JobQueued, "restic_backup", w1.ID}:       {1, t0()},
		{domain.JobQueued, "rclone_transfer", w2.ID}:     {1, t0()},
		{domain.JobDispatched, "rclone_transfer", w1.ID}: {1, t0()},
		{domain.JobRunning, "rclone_transfer", w1.ID}:    {1, t0().Add(-time.Hour)},
	}
	check("owner", owner, mine)
	all := maps.Clone(mine)
	all[key{domain.JobQueued, "rclone_transfer", w1.ID}] = val{3, t0().Add(-time.Minute)}
	check("every owner", domain.UserID{}, all)
	check("nobody", userID(t, 99), map[key]val{})
}

// testActivityTotals checks JobTotals (groups, run-time buckets, open and
// closed ranges) and the run-time and queue-wait sums of JobActivity.
func testActivityTotals(t *testing.T, s Store) {
	ctx := context.Background()
	requireActivity(t, s)
	owner, other := ownerID(t), userID(t, 2)
	ensureUser(t, s, owner)
	ensureUser(t, s, other)
	w1, w2 := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	at := func(d time.Duration) time.Time { return t0().Add(d) }
	n := 0
	finish := func(by domain.UserID, w domain.WorkerID, kind string, st domain.JobStatus, code string,
		dispatched, started, finished time.Time, bytes, files int64) {
		t.Helper()
		n++
		j := newJob(t, n, w)
		j.OwnerID, j.Kind = by, kind
		mustCreateJob(t, s, j)
		j.Status, j.Error.Code = st, code
		if code != "" {
			j.Error.Message = "boom"
		}
		j.DispatchedAt, j.StartedAt, j.FinishedAt = dispatched, started, finished
		j.Stats = jobs.Stats{Bytes: bytes, Files: files}
		if !dispatched.IsZero() {
			j.Epoch = "e1"
		}
		if _, err := s.UpdateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	const (
		transfer = "rclone_transfer"
		backup   = "restic_backup"
	)
	none := time.Time{}
	// Created at t0. Run times: 60 s, 61 s, 3 h, 999.999 ms (the times are
	// cut to whole microseconds first), none (never started), none
	// (finished before it started); queue waits 5 s, 5 s, 0, 1 s, none,
	// none (dispatched before it was created).
	finish(owner, w1.ID, transfer, domain.JobSucceeded, "", at(5*time.Second), at(10*time.Second), at(70*time.Second), 100, 2)
	finish(owner, w1.ID, transfer, domain.JobSucceeded, "", at(5*time.Second), at(10*time.Second), at(71*time.Second), 10, 1)
	finish(owner, w1.ID, transfer, domain.JobFailed, "execution_failed", at(0), at(0), at(3*time.Hour), 5, 0)
	finish(owner, w2.ID, backup, domain.JobSucceeded, "", at(time.Second), at(time.Second+1500*time.Nanosecond), at(2*time.Second+999*time.Nanosecond), 7, 3)
	finish(owner, w1.ID, transfer, domain.JobCancelled, "cancelled", none, none, at(time.Second), 0, 0)
	finish(owner, w2.ID, transfer, domain.JobInterrupted, "worker_restarted", at(-time.Second), at(100*time.Second), at(50*time.Second), 1, 1)
	finish(other, w1.ID, transfer, domain.JobSucceeded, "", at(5*time.Second), at(10*time.Second), at(70*time.Second), 1000, 1)
	n++
	mustCreateJob(t, s, newJob(t, n, w1.ID)) // queued: never counted

	type key struct {
		kind   string
		w      domain.WorkerID
		st     domain.JobStatus
		code   string
		bucket int
	}
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	us := func(d time.Duration) int64 { return d.Microseconds() }
	check := func(name string, q jobs.TotalsQuery, want map[key]jobs.Sums) {
		t.Helper()
		rows, err := s.JobTotals(ctx, q)
		if err != nil {
			t.Fatalf("%s: JobTotals: %v", name, err)
		}
		got := map[key]jobs.Sums{}
		for _, r := range rows {
			k := key{r.Kind, r.WorkerID, r.Status, r.ErrorCode, r.RunTimeBucket}
			if _, dup := got[k]; dup {
				t.Errorf("%s: group %+v twice", name, k)
			}
			got[k] = r.Sums
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: JobTotals =\n%v\nwant\n%v", name, got, want)
		}
	}
	bounds := []time.Duration{time.Minute, time.Hour}
	mine := map[key]jobs.Sums{
		{transfer, w1.ID, domain.JobSucceeded, "", 0}:                    {Jobs: 1, Bytes: 100, Files: 2, Started: 1, RunTimeMicros: us(sec(60)), Dispatched: 1, QueueWaitMicros: us(sec(5))},
		{transfer, w1.ID, domain.JobSucceeded, "", 1}:                    {Jobs: 1, Bytes: 10, Files: 1, Started: 1, RunTimeMicros: us(sec(61)), Dispatched: 1, QueueWaitMicros: us(sec(5))},
		{transfer, w1.ID, domain.JobFailed, "execution_failed", 2}:       {Jobs: 1, Bytes: 5, Started: 1, RunTimeMicros: us(3 * time.Hour), Dispatched: 1},
		{backup, w2.ID, domain.JobSucceeded, "", 0}:                      {Jobs: 1, Bytes: 7, Files: 3, Started: 1, RunTimeMicros: us(time.Second - time.Microsecond), Dispatched: 1, QueueWaitMicros: us(sec(1))},
		{transfer, w1.ID, domain.JobCancelled, "cancelled", -1}:          {Jobs: 1},
		{transfer, w2.ID, domain.JobInterrupted, "worker_restarted", -1}: {Jobs: 1, Bytes: 1, Files: 1},
	}
	check("owner, all time", jobs.TotalsQuery{OwnerID: owner, RunTimeBounds: bounds}, mine)
	all := maps.Clone(mine)
	all[key{transfer, w1.ID, domain.JobSucceeded, "", 0}] = jobs.Sums{Jobs: 2, Bytes: 1100, Files: 3, Started: 2, RunTimeMicros: us(sec(120)), Dispatched: 2, QueueWaitMicros: us(sec(10))}
	check("every owner", jobs.TotalsQuery{RunTimeBounds: bounds}, all)
	check("from inclusive, until exclusive", jobs.TotalsQuery{OwnerID: owner, From: at(70 * time.Second), Until: at(71 * time.Second), RunTimeBounds: bounds},
		map[key]jobs.Sums{{transfer, w1.ID, domain.JobSucceeded, "", 0}: mine[key{transfer, w1.ID, domain.JobSucceeded, "", 0}]})
	check("open start", jobs.TotalsQuery{OwnerID: owner, Until: at(2 * time.Second), RunTimeBounds: bounds},
		map[key]jobs.Sums{{transfer, w1.ID, domain.JobCancelled, "cancelled", -1}: {Jobs: 1}})
	check("open end", jobs.TotalsQuery{OwnerID: owner, From: at(time.Hour)},
		map[key]jobs.Sums{{transfer, w1.ID, domain.JobFailed, "execution_failed", 0}: mine[key{transfer, w1.ID, domain.JobFailed, "execution_failed", 2}]})
	merged := map[key]jobs.Sums{
		{transfer, w1.ID, domain.JobSucceeded, "", 0}:                    {Jobs: 2, Bytes: 110, Files: 3, Started: 2, RunTimeMicros: us(sec(121)), Dispatched: 2, QueueWaitMicros: us(sec(10))},
		{transfer, w1.ID, domain.JobFailed, "execution_failed", 0}:       mine[key{transfer, w1.ID, domain.JobFailed, "execution_failed", 2}],
		{backup, w2.ID, domain.JobSucceeded, "", 0}:                      mine[key{backup, w2.ID, domain.JobSucceeded, "", 0}],
		{transfer, w1.ID, domain.JobCancelled, "cancelled", -1}:          {Jobs: 1},
		{transfer, w2.ID, domain.JobInterrupted, "worker_restarted", -1}: {Jobs: 1, Bytes: 1, Files: 1},
	}
	check("no bounds", jobs.TotalsQuery{OwnerID: owner}, merged)
	check("nobody", jobs.TotalsQuery{OwnerID: userID(t, 99)}, map[key]jobs.Sums{})

	// JobActivity sums the same run times and waits by bucket and status.
	rows, err := s.JobActivity(ctx, jobs.ActivityQuery{From: at(-time.Hour), Until: at(4 * time.Hour), Unit: jobs.BucketDay, Location: time.UTC, OwnerID: owner})
	if err != nil {
		t.Fatal(err)
	}
	got := map[domain.JobStatus]jobs.Sums{}
	for _, r := range rows {
		got[r.Status] = r.Sums
	}
	want := map[domain.JobStatus]jobs.Sums{
		domain.JobSucceeded:   {Jobs: 3, Bytes: 117, Files: 6, Started: 3, RunTimeMicros: us(sec(122) - time.Microsecond), Dispatched: 3, QueueWaitMicros: us(sec(11))},
		domain.JobFailed:      {Jobs: 1, Bytes: 5, Started: 1, RunTimeMicros: us(3 * time.Hour), Dispatched: 1},
		domain.JobCancelled:   {Jobs: 1},
		domain.JobInterrupted: {Jobs: 1, Bytes: 1, Files: 1},
	}
	if !maps.Equal(got, want) {
		t.Errorf("JobActivity sums =\n%v\nwant\n%v", got, want)
	}
}
