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
	_, err := s.CountUnfinished(context.Background(), domain.UserID{})
	skipIfUnsupported(t, "CountUnfinished", "track C (dashboard)", err)
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

func testActivityCountUnfinished(t *testing.T, s Store) {
	ctx := context.Background()
	requireActivity(t, s)
	owner, other := ownerID(t), userID(t, 2)
	ensureUser(t, s, owner)
	ensureUser(t, s, other)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	statuses := []struct {
		st domain.JobStatus
		by domain.UserID
	}{
		{domain.JobQueued, owner}, {domain.JobQueued, owner}, {domain.JobDispatched, owner}, {domain.JobRunning, owner},
		{domain.JobSucceeded, owner}, {domain.JobFailed, owner}, {domain.JobQueued, other},
	}
	for i, tt := range statuses {
		j := newJob(t, i+1, w.ID)
		j.OwnerID = tt.by
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
	got, err := s.CountUnfinished(ctx, owner)
	if want := map[domain.JobStatus]int64{domain.JobQueued: 2, domain.JobDispatched: 1, domain.JobRunning: 1}; err != nil || !maps.Equal(got, want) {
		t.Errorf("CountUnfinished(owner) = (%v, %v), want %v", got, err, want)
	}
	got, err = s.CountUnfinished(ctx, domain.UserID{})
	if want := map[domain.JobStatus]int64{domain.JobQueued: 3, domain.JobDispatched: 1, domain.JobRunning: 1}; err != nil || !maps.Equal(got, want) {
		t.Errorf("CountUnfinished(all) = (%v, %v), want %v", got, err, want)
	}
	if got, err := s.CountUnfinished(ctx, userID(t, 99)); err != nil || len(got) != 0 {
		t.Errorf("CountUnfinished(nobody) = (%v, %v)", got, err)
	}
}
