package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// seen marks a worker (one that does not heartbeat in the env) last seen
// ago before the env's clock.
func (e *env) seen(id domain.WorkerID, ago time.Duration) {
	e.t.Helper()
	if err := e.store.TouchWorker(context.Background(), id, e.clock.Now().Add(-ago)); err != nil {
		e.t.Fatal(err)
	}
}

// TestRunWaitsForAQuietWorker: a worker last seen 31 to 45 seconds ago is
// still shown online but is not launched on, so the run waits; once it is
// seen within 30 seconds the run launches.
func TestRunWaitsForAQuietWorker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "laptop"})
	sc := e.create(e.input("nightly", schedules.Target{WorkerID: w}))
	s := e.newScheduler(nil, nil)
	e.clock.Set(t0.Add(time.Hour))

	for _, ago := range []time.Duration{31 * time.Second, 40 * time.Second, 44 * time.Second} {
		e.seen(w, ago)
		if st := e.workers.Status(e.worker(w)); st != domain.WorkerOnline {
			t.Fatalf("seen %v ago: status %s, want online (the test needs a worker shown online)", ago, st)
		}
		e.tick(s)
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending || len(e.jobsOf(sc.ID)) != 0 {
			t.Fatalf("seen %v ago: run %+v with %d jobs, want it waiting", ago, r, len(e.jobsOf(sc.ID)))
		}
		e.clock.Advance(time.Minute)
	}
	e.seen(w, 29*time.Second)
	e.tick(s)
	if r, js := e.onlyRun(sc.ID), e.jobsOf(sc.ID); r.Status != schedules.RunLaunched || len(js) != 1 || js[0].WorkerID != w {
		t.Fatalf("seen 29s ago: run %+v, jobs %+v, want launched", r, js)
	}
}

// TestLabelSkipsQuietWorkers: among a label's workers only the reachable
// ones are candidates, however idle the quiet ones are; with only quiet
// ones the run waits instead of failing.
func TestLabelSkipsQuietWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	quiet := e.addWorker(worker{name: "a-quiet", labels: []string{"nas"}})
	fresh := e.addWorker(worker{name: "b-fresh", labels: []string{"nas"}})
	e.busyJob(fresh) // the quiet one would win on load
	sc := e.create(e.input("by label", schedules.Target{Label: "nas"}))
	s := e.newScheduler(nil, nil)
	e.clock.Set(t0.Add(time.Hour))

	e.seen(quiet, 35*time.Second)
	e.seen(fresh, 35*time.Second)
	e.tick(s)
	if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending || len(e.jobsOf(sc.ID)) != 0 {
		t.Fatalf("both quiet: run %+v, want it waiting", r)
	}
	e.seen(quiet, 35*time.Second)
	e.seen(fresh, 10*time.Second)
	e.tick(s)
	if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != fresh {
		t.Fatalf("jobs = %+v, want one on the reachable worker", js)
	}
}

func (e *env) worker(id domain.WorkerID) workers.Worker {
	e.t.Helper()
	w, err := e.workers.Get(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}
