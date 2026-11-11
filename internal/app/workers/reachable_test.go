package workers_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// TestReachableIsSeenWithinLaunchWithin: a worker is reachable for
// LaunchWithin after its last contact, while it still counts as online
// until OfflineAfter; never connected is unreachable; a shorter
// OfflineAfter bounds it.
func TestReachableIsSeenWithinLaunchWithin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	id := e.enroll(t, "w").Worker.ID
	get := func() workers.Worker {
		t.Helper()
		w, err := e.svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if e.svc.Reachable(get()) {
		t.Error("a worker that never connected is reachable")
	}
	if err := e.svc.Touch(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		after     time.Duration
		reachable bool
		status    domain.WorkerStatus
	}{
		{0, true, domain.WorkerOnline},
		{29 * time.Second, true, domain.WorkerOnline},
		{30 * time.Second, false, domain.WorkerOnline},
		{44 * time.Second, false, domain.WorkerOnline},
		{45 * time.Second, false, domain.WorkerOffline},
	} {
		w := get()
		e.clock.Set(w.Link.LastSeen.Add(tt.after))
		if got := e.svc.Reachable(w); got != tt.reachable {
			t.Errorf("%s after the last contact: Reachable = %v, want %v", tt.after, got, tt.reachable)
		}
		if got := e.svc.Status(w); got != tt.status {
			t.Errorf("%s after the last contact: Status = %s, want %s", tt.after, got, tt.status)
		}
	}
	w := get()
	e.clock.Set(w.Link.LastSeen.Add(-time.Minute))
	if !e.svc.Reachable(w) {
		t.Error("a last contact in the future is not reachable")
	}
	if workers.LaunchWithin != 30*time.Second {
		t.Errorf("LaunchWithin = %s, want 30s", workers.LaunchWithin)
	}

	// A shorter OfflineAfter is the bound: a worker shown offline is never
	// reachable.
	clk := clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	store := memstore.New()
	short, err := workers.New(workers.Deps{
		Workers: store, Tokens: store, Cipher: e.cipher, Clock: clk,
		IDs: &idgen.Sequential{}, Jobs: e.jobs, Random: rand.Reader,
	}, workers.Config{OfflineAfter: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	seen := workers.Worker{Link: workers.Link{LastSeen: clk.Now()}}
	clk.Advance(19 * time.Second)
	if !short.Reachable(seen) {
		t.Error("19s after the last contact with OfflineAfter 20s: not reachable")
	}
	clk.Advance(time.Second)
	if short.Reachable(seen) || short.Status(seen) != domain.WorkerOffline {
		t.Errorf("20s after the last contact with OfflineAfter 20s: reachable %v, status %s", short.Reachable(seen), short.Status(seen))
	}
}
