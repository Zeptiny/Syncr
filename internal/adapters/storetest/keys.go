package storetest

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// keyedJob returns queued job n of worker w that holds the concurrency
// keys, created (and queued) at created.
func keyedJob(t *testing.T, n int, w domain.WorkerID, created time.Time, keys ...string) jobs.Job {
	j := newJob(t, n, w)
	j.CreatedAt, j.QueuedAt = created, created
	j.ConcurrencyKeys = keys
	return j
}

// setJobStatus moves a stored job to st (finished at finishedAt, if set).
func setJobStatus(t *testing.T, s Store, id domain.JobID, st domain.JobStatus, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	j, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	j.Status, j.FinishedAt = st, finishedAt
	if _, err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
}

// testJobConcurrencyKeys: keys are stored as given and returned by every
// read; a job without keys has none.
func testJobConcurrencyKeys(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	keyed := keyedJob(t, 1, w.ID, t0(), "restic-repo:a", "restic-repo:b")
	plain := newJob(t, 2, w.ID)
	mustCreateJob(t, s, keyed)
	mustCreateJob(t, s, plain)

	got, err := s.GetJob(ctx, keyed.ID)
	if err != nil || !slices.Equal(got.ConcurrencyKeys, keyed.ConcurrencyKeys) {
		t.Fatalf("GetJob keys = %v, %v", got.ConcurrencyKeys, err)
	}
	if got, err := s.GetJob(ctx, plain.ID); err != nil || len(got.ConcurrencyKeys) != 0 {
		t.Fatalf("a job without keys has %v, %v", got.ConcurrencyKeys, err)
	}
	// An update never changes them.
	got.ConcurrencyKeys = []string{"other"}
	got.Status = domain.JobDispatched
	if stored, err := s.UpdateJob(ctx, got); err != nil || !slices.Equal(stored.ConcurrencyKeys, keyed.ConcurrencyKeys) {
		t.Fatalf("after UpdateJob keys = %v, %v", stored.ConcurrencyKeys, err)
	}
	list, err := s.ListUnfinishedJobs(ctx, w.ID)
	if err != nil || len(list) != 2 || !slices.Equal(list[0].ConcurrencyKeys, keyed.ConcurrencyKeys) {
		t.Fatalf("ListUnfinishedJobs = %+v, %v", list, err)
	}
	// Returned keys do not alias the store.
	list[0].ConcurrencyKeys[0] = "mutated"
	if again, _ := s.GetJob(ctx, keyed.ID); again.ConcurrencyKeys[0] != "restic-repo:a" {
		t.Error("returned keys alias the store")
	}
}

// testJobBlockers: a keyed job's blocker is the oldest unfinished older
// job sharing one of its keys, by (CreatedAt, ID).
func testJobBlockers(t *testing.T, s Store) {
	ctx := context.Background()
	w1, w2 := newWorker(t, 1), newWorker(t, 2)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	at := t0()

	// IDs are in creation order except job 9, whose ID is the greatest but
	// which was created first: (CreatedAt, ID) decides, not the ID alone.
	first := keyedJob(t, 9, w2.ID, at.Add(-time.Minute), "k1")
	a := keyedJob(t, 1, w1.ID, at, "k1")
	b := keyedJob(t, 2, w2.ID, at, "k1", "k2") // same instant as a: the ID breaks the tie
	c := keyedJob(t, 3, w1.ID, at.Add(time.Second), "k2")
	d := keyedJob(t, 4, w1.ID, at.Add(time.Second), "k3")
	plain := newJob(t, 5, w1.ID)
	for _, j := range []jobs.Job{first, a, b, c, d, plain} {
		mustCreateJob(t, s, j)
	}
	all := []domain.JobID{first.ID, a.ID, b.ID, c.ID, d.ID, plain.ID, jobID(t, 77)}

	check := func(what string, want map[domain.JobID]domain.JobID) {
		t.Helper()
		got, err := s.ListJobBlockers(ctx, all)
		if err != nil {
			t.Fatalf("%s: ListJobBlockers: %v", what, err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("%s: blockers = %v, want %v", what, got, want)
		}
	}
	check("all queued", map[domain.JobID]domain.JobID{a.ID: first.ID, b.ID: first.ID, c.ID: b.ID})

	// Dispatched and running jobs block like queued ones.
	setJobStatus(t, s, first.ID, domain.JobDispatched, time.Time{})
	check("head dispatched", map[domain.JobID]domain.JobID{a.ID: first.ID, b.ID: first.ID, c.ID: b.ID})
	setJobStatus(t, s, first.ID, domain.JobRunning, time.Time{})
	check("head running", map[domain.JobID]domain.JobID{a.ID: first.ID, b.ID: first.ID, c.ID: b.ID})

	// Every terminal status frees the keys.
	setJobStatus(t, s, first.ID, domain.JobSucceeded, at.Add(time.Minute))
	check("head finished", map[domain.JobID]domain.JobID{b.ID: a.ID, c.ID: b.ID})
	setJobStatus(t, s, a.ID, domain.JobCancelled, at.Add(time.Minute))
	check("second cancelled", map[domain.JobID]domain.JobID{c.ID: b.ID})
	setJobStatus(t, s, b.ID, domain.JobFailed, at.Add(time.Minute))
	check("third failed", map[domain.JobID]domain.JobID{})

	// A younger job never blocks an older one.
	late := keyedJob(t, 6, w2.ID, at.Add(time.Hour), "k2", "k3")
	mustCreateJob(t, s, late)
	got, err := s.ListJobBlockers(ctx, []domain.JobID{c.ID, d.ID, late.ID})
	if err != nil || len(got) != 1 || got[late.ID] != c.ID {
		t.Fatalf("with a late job: blockers = %v, %v; want it to wait for the oldest sharer", got, err)
	}
	if got, err := s.ListJobBlockers(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no ids: %v, %v", got, err)
	}
}

// testJobQueuedIsKeyAware: the dispatch-timeout candidates leave out a
// keyed job behind an older sharer that is unfinished or finished at or
// after the cutoff.
func testJobQueuedIsKeyAware(t *testing.T, s Store) {
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	at := t0()
	cutoff := at.Add(10 * time.Minute)

	head := keyedJob(t, 1, w.ID, at, "k1")
	waiter := keyedJob(t, 2, w.ID, at.Add(time.Second), "k1")
	otherKey := keyedJob(t, 3, w.ID, at.Add(2*time.Second), "k2")
	plain := newJob(t, 4, w.ID)
	plain.CreatedAt, plain.QueuedAt = at.Add(3*time.Second), at.Add(3*time.Second)
	young := keyedJob(t, 5, w.ID, cutoff.Add(time.Minute), "k2") // queued after the cutoff
	for _, j := range []jobs.Job{head, waiter, otherKey, plain, young} {
		mustCreateJob(t, s, j)
	}

	check := func(what string, want ...domain.JobID) {
		t.Helper()
		got, err := s.ListQueuedJobs(ctx, cutoff, -1)
		if err != nil {
			t.Fatalf("%s: ListQueuedJobs: %v", what, err)
		}
		if ids := jobIDs(got); !slices.Equal(ids, want) {
			t.Fatalf("%s: candidates = %v, want %v", what, ids, want)
		}
	}
	check("head queued", head.ID, otherKey.ID, plain.ID)

	setJobStatus(t, s, head.ID, domain.JobRunning, time.Time{})
	check("head running", otherKey.ID, plain.ID)

	// The head finished after the cutoff: the waiter became the head too
	// recently.
	setJobStatus(t, s, head.ID, domain.JobSucceeded, cutoff.Add(time.Second))
	check("head finished after the cutoff", otherKey.ID, plain.ID)
	// Exactly at the cutoff still counts as too recent.
	setJobStatusFinishedAt(t, s, head.ID, cutoff)
	check("head finished at the cutoff", otherKey.ID, plain.ID)
	// Before the cutoff: the waiter has been the head for the full time.
	setJobStatusFinishedAt(t, s, head.ID, cutoff.Add(-time.Second))
	check("head finished before the cutoff", waiter.ID, otherKey.ID, plain.ID)

	if got, err := s.ListQueuedJobs(ctx, cutoff, 2); err != nil || !slices.Equal(jobIDs(got), []domain.JobID{waiter.ID, otherKey.ID}) {
		t.Fatalf("limit 2: %v, %v", jobIDs(got), err)
	}
}

// setJobStatusFinishedAt rewrites a finished job's FinishedAt.
func setJobStatusFinishedAt(t *testing.T, s Store, id domain.JobID, at time.Time) {
	t.Helper()
	ctx := context.Background()
	j, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	j.FinishedAt = at
	if _, err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
}

// testJobWorkersWaitingOnKeys: the workers of the queued jobs that share a
// key, each once, by ID.
func testJobWorkersWaitingOnKeys(t *testing.T, s Store) {
	ctx := context.Background()
	w1, w2, w3 := newWorker(t, 1), newWorker(t, 2), newWorker(t, 3)
	mustCreateWorker(t, s, w1)
	mustCreateWorker(t, s, w2)
	mustCreateWorker(t, s, w3)
	at := t0()
	running := keyedJob(t, 1, w3.ID, at, "k1")
	for _, j := range []jobs.Job{
		running,
		keyedJob(t, 2, w2.ID, at, "k1"),
		keyedJob(t, 3, w2.ID, at, "k1", "k2"),
		keyedJob(t, 4, w1.ID, at, "k2"),
		keyedJob(t, 5, w1.ID, at, "k3"),
		newJob(t, 6, w3.ID),
	} {
		mustCreateJob(t, s, j)
	}
	setJobStatus(t, s, running.ID, domain.JobRunning, time.Time{})

	for _, tt := range []struct {
		keys []string
		want []domain.WorkerID
	}{
		{[]string{"k1"}, []domain.WorkerID{w2.ID}}, // not w3: its job runs
		{[]string{"k2"}, []domain.WorkerID{w1.ID, w2.ID}},
		{[]string{"k1", "k2", "k3"}, []domain.WorkerID{w1.ID, w2.ID}},
		{[]string{"k3"}, []domain.WorkerID{w1.ID}},
		{[]string{"unknown"}, nil},
		{nil, nil},
	} {
		got, err := s.ListWorkersWaitingOnKeys(ctx, tt.keys)
		if err != nil {
			t.Fatalf("ListWorkersWaitingOnKeys(%v): %v", tt.keys, err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("ListWorkersWaitingOnKeys(%v) = %v, want %v", tt.keys, got, tt.want)
		}
	}
}
