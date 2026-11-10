package jobs_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func (e *env) keyed(t *testing.T, worker domain.WorkerID, keys ...string) jobs.Job {
	t.Helper()
	return e.createAs(t, e.owner, jobs.CreateParams{WorkerID: worker, ConcurrencyKeys: keys})
}

func (r *recorder) wokenSince(n int) []domain.WorkerID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.wakes[n:])
}

func (r *recorder) wakeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.wakes)
}

func TestCreateNormalizesConcurrencyKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	j := e.keyed(t, e.worker, "restic-repo:b", "restic-repo:a", "restic-repo:b")
	if want := []string{"restic-repo:a", "restic-repo:b"}; !slices.Equal(j.ConcurrencyKeys, want) {
		t.Errorf("keys = %v, want %v", j.ConcurrencyKeys, want)
	}
	if got := e.create(t).ConcurrencyKeys; got != nil {
		t.Errorf("a job without keys has %v", got)
	}

	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "k" + string(rune('a'+i))
	}
	for name, keys := range map[string][]string{
		"empty key":   {""},
		"upper case":  {"Restic"},
		"leading dot": {".x"},
		"space":       {"a b"},
		"too long":    {strings.Repeat("a", 201)},
		"too many":    nine,
	} {
		_, err := e.svc.Create(context.Background(), e.owner, jobs.CreateParams{
			WorkerID: e.worker, Kind: "rclone_transfer", Definition: []byte(testDefinition), ConcurrencyKeys: keys,
		})
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if !jobs.ValidConcurrencyKey("restic-repo:0190a8e2-0000-7000-8000-000000000001") || !jobs.ValidConcurrencyKey(strings.Repeat("a", 200)) {
		t.Error("valid keys rejected")
	}
}

// TestBlockers: a keyed job waits for the oldest older unfinished job that
// shares one of its keys; creation order decides, with the ID breaking
// ties of the creation time.
func TestBlockers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	other := e.addWorker(t)

	// All four are created at the same instant: the IDs order them.
	a := e.keyed(t, e.worker, "k1")
	b := e.keyed(t, other, "k1", "k2")
	c := e.keyed(t, e.worker, "k2")
	free := e.create(t)
	d := e.keyed(t, other, "k3")

	blockers := func() map[domain.JobID]domain.JobID {
		t.Helper()
		m, err := e.svc.Blockers(ctx, []domain.JobID{a.ID, b.ID, c.ID, free.ID, d.ID, a.ID})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	want := map[domain.JobID]domain.JobID{b.ID: a.ID, c.ID: b.ID}
	if got := blockers(); !mapsEqual(got, want) {
		t.Fatalf("blockers = %v, want %v", got, want)
	}

	// A running head still blocks; a finished one does not.
	if _, _, err := e.svc.MarkRunning(ctx, e.worker, a.ID, epoch, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := blockers(); !mapsEqual(got, want) {
		t.Fatalf("with a running: blockers = %v, want %v", got, want)
	}
	if _, _, err := e.svc.Finish(ctx, e.worker, a.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if got, want := blockers(), map[domain.JobID]domain.JobID{c.ID: b.ID}; !mapsEqual(got, want) {
		t.Fatalf("after a finished: blockers = %v, want %v", got, want)
	}

	// A job created later never blocks an older one, whatever its state.
	late := e.keyed(t, e.worker, "k1", "k2", "k3")
	if got, err := e.svc.Blockers(ctx, []domain.JobID{late.ID, d.ID}); err != nil || got[late.ID] != b.ID || len(got) != 1 {
		t.Fatalf("late: blockers = %v, %v; want it to wait for b (the oldest sharer)", got, err)
	}

	if m, err := e.svc.Blockers(ctx, nil); err != nil || len(m) != 0 {
		t.Errorf("no ids: %v, %v", m, err)
	}
	if _, err := e.svc.Blockers(ctx, make([]domain.JobID, jobs.MaxBlockerIDs+1)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("too many ids: %v", err)
	}
}

func mapsEqual(a, b map[domain.JobID]domain.JobID) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestFinishedKeyedJobWakesWaitingWorkers: when a job holding keys ends,
// the workers of the queued jobs sharing them are woken, so the next head
// starts without waiting for a poll.
func TestFinishedKeyedJobWakesWaitingWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w2, w3, w4 := e.addWorker(t), e.addWorker(t), e.addWorker(t)

	head := e.keyed(t, e.worker, "k1", "k2")
	e.keyed(t, w2, "k1")
	e.keyed(t, w2, "k2")
	e.keyed(t, w3, "k2")
	e.keyed(t, w4, "other")
	unkeyed := e.createAs(t, e.owner, jobs.CreateParams{WorkerID: w4})

	before := e.rec.wakeCount()
	if _, _, err := e.svc.Finish(ctx, w4, unkeyed.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if got := e.rec.wokenSince(before); len(got) != 0 {
		t.Errorf("a job without keys woke %v", got)
	}

	before = e.rec.wakeCount()
	if _, _, err := e.svc.Finish(ctx, e.worker, head.ID, epoch, jobs.Outcome{Status: domain.JobFailed}); err != nil {
		t.Fatal(err)
	}
	got := e.rec.wokenSince(before)
	want := []domain.WorkerID{w2, w3}
	sortIDs := func(ids []domain.WorkerID) {
		slices.SortFunc(ids, func(a, b domain.WorkerID) int { return strings.Compare(a.String(), b.String()) })
	}
	sortIDs(got)
	sortIDs(want)
	if !slices.Equal(got, want) {
		t.Errorf("woken = %v, want %v (each once)", got, want)
	}

	// Canceling a queued keyed job is a terminal change too.
	queued := e.keyed(t, e.worker, "k9")
	e.keyed(t, w3, "k9")
	before = e.rec.wakeCount()
	if _, err := e.svc.Cancel(ctx, e.owner, queued.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.rec.wokenSince(before); !slices.Equal(got, []domain.WorkerID{w3}) {
		t.Errorf("after a cancel: woken = %v, want [%v]", got, w3)
	}
}

// TestQueuedBeforeIsKeyAware: a keyed job waiting behind an older sharer
// is no dispatch-timeout candidate, and its clock starts when it becomes
// the head of its keys.
func TestQueuedBeforeIsKeyAware(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	head := e.keyed(t, e.worker, "k")
	waiter := e.keyed(t, e.worker, "k")
	plain := e.create(t)

	candidates := func(before time.Time) []domain.JobID {
		t.Helper()
		js, err := e.svc.QueuedBefore(ctx, before, -1)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]domain.JobID, len(js))
		for i, j := range js {
			ids[i] = j.ID
		}
		return ids
	}

	e.clock.Advance(20 * time.Minute)
	cutoff := e.clock.Now().Add(-15 * time.Minute)
	if got, want := candidates(cutoff), []domain.JobID{head.ID, plain.ID}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v (the waiter is behind the head)", got, want)
	}

	// The head ends now: the waiter became the head only now, so it is no
	// candidate until a full timeout later.
	if _, err := e.svc.Cancel(ctx, e.owner, head.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := candidates(cutoff), []domain.JobID{plain.ID}; !slices.Equal(got, want) {
		t.Fatalf("right after the head ended: candidates = %v, want %v", got, want)
	}
	e.clock.Advance(15*time.Minute + time.Second)
	cutoff = e.clock.Now().Add(-15 * time.Minute)
	if got, want := candidates(cutoff), []domain.JobID{waiter.ID, plain.ID}; !slices.Equal(got, want) {
		t.Fatalf("a timeout after the head ended: candidates = %v, want %v", got, want)
	}
}
