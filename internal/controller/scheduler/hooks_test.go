package scheduler_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// hookedInput is backupInput whose job stops and starts an app around the
// backup.
func (e *env) hookedInput(name string, target schedules.Target, repo domain.ResticRepoID) schedules.Input {
	in := e.backupInput(name, target, repo)
	in.Definition = fmt.Appendf(nil,
		`{"hooks":{"before":["stop-app"],"after":["start-app"]},"restic_backup":{"repository_id":%q,"paths":["/data"]}}`, repo)
	return in
}

// withoutHooksFeature is every link feature but the one for hooks: a worker
// from before hooks existed.
func withoutHooksFeature() []string {
	return slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks })
}

// TestTargetsDefineTheHooks: a definition that names hooks only goes to
// workers that define them all. A label's other workers are skipped; a
// label without one fails the run (the schedule stays enabled); a chosen
// worker that lacks one, or cannot run hooks at all, fails it and pauses
// the schedule. A worker that has not reported yet may still take it.
func TestTargetsDefineTheHooks(t *testing.T) {
	t.Parallel()
	backup := func(name string, online bool, hooks []string, labels ...string) worker {
		return worker{name: name, online: online, labels: labels, kinds: []string{"restic_backup"}, features: link.Features(), os: "linux", hooks: hooks}
	}
	both := []string{"start-app", "stop-app", "unused"}

	t.Run("label: only the worker with the hooks", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-bare", true, nil, "app"))                  // idle and first by name
		e.addWorker(backup("b-half", true, []string{"stop-app"}, "app")) // lacks start-app
		old := backup("c-old", true, both, "app")
		old.features = withoutHooksFeature() // whatever hooks it claims
		e.addWorker(old)
		nas := e.addWorker(backup("d-nas", true, both, "app"))
		e.busyJob(nas)
		sc := e.create(e.hookedInput("nightly", schedules.Target{Label: "app"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != nas {
			t.Fatalf("jobs = %+v, want one on the worker that defines both hooks", js)
		}
	})

	t.Run("label: no worker with the hooks", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-bare", true, nil, "app"))
		e.addWorker(backup("b-half", false, []string{"start-app"}, "app"))
		sc := e.create(e.hookedInput("nightly", schedules.Target{Label: "app"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		r := e.onlyRun(sc.ID)
		if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonNoWorkerWithLabel ||
			r.Message != `no worker with the label "app" can run this job: none defines the hooks "stop-app", "start-app" it names (a worker's operator adds hooks in its configuration)` {
			t.Fatalf("run = %+v", r)
		}
		if !e.schedule(sc.ID).Enabled || len(e.jobsOf(sc.ID)) != 0 {
			t.Errorf("schedule enabled %v, jobs %d", e.schedule(sc.ID).Enabled, len(e.jobsOf(sc.ID)))
		}
	})

	t.Run("label: the worker with the hooks is offline: waits", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-bare", true, nil, "app"))
		e.addWorker(backup("b-nas", false, both, "app"))
		sc := e.create(e.hookedInput("nightly", schedules.Target{Label: "app"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending || len(e.jobsOf(sc.ID)) != 0 {
			t.Fatalf("run = %+v, want it waiting for the worker that defines the hooks", r)
		}
	})

	t.Run("label: a worker that has not reported yet may", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-bare", true, nil, "app"))
		e.addWorker(worker{name: "new", online: false, labels: []string{"app"}, kinds: []string{}}) // never reported
		sc := e.create(e.hookedInput("nightly", schedules.Target{Label: "app"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending {
			t.Fatalf("run = %+v, want it waiting", r)
		}
	})

	t.Run("label: a job without hooks goes anywhere", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		old := backup("a-old", true, nil, "app")
		old.features = withoutHooksFeature()
		id := e.addWorker(old)
		sc := e.create(e.backupInput("nightly", schedules.Target{Label: "app"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != id {
			t.Fatalf("jobs = %+v, want one on the worker", js)
		}
	})

	t.Run("worker: with the hooks", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.addWorker(backup("nas", true, both))
		sc := e.create(e.hookedInput("nightly", schedules.Target{WorkerID: w}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != w {
			t.Fatalf("jobs = %+v, want one on the worker", js)
		}
	})

	t.Run("worker: lacks a hook", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.addWorker(backup("nas", true, []string{"stop-app"}))
		sc := e.create(e.hookedInput("nightly", schedules.Target{WorkerID: w}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		r := e.onlyRun(sc.ID)
		if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonInvalidDefinition ||
			r.Message != `the schedule's worker "nas" defines no hook "start-app". Add what is missing to the worker's configuration (hooks) and restart it, or change the job, then resume the schedule` {
			t.Fatalf("run = %+v", r)
		}
		if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonInvalidDefinition || len(e.jobsOf(sc.ID)) != 0 {
			t.Errorf("schedule enabled %v, paused reason %q, jobs %d", got.Enabled, got.PausedReason, len(e.jobsOf(sc.ID)))
		}
	})

	t.Run("worker: cannot run hooks", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		old := backup("old", true, nil)
		old.features = withoutHooksFeature()
		w := e.addWorker(old)
		sc := e.create(e.hookedInput("nightly", schedules.Target{WorkerID: w}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		r := e.onlyRun(sc.ID)
		if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonWorkerOutdated ||
			r.Message != `the schedule's worker "old" (version unknown) cannot run hooks. Upgrade it, or remove the hooks from the job, then resume the schedule` {
			t.Fatalf("run = %+v", r)
		}
		if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonWorkerOutdated || len(e.jobsOf(sc.ID)) != 0 {
			t.Errorf("schedule enabled %v, paused reason %q, jobs %d", got.Enabled, got.PausedReason, len(e.jobsOf(sc.ID)))
		}
	})

	// What an unreachable worker reported may be a previous process's (its
	// operator may be adding the hook right now): the run waits.
	t.Run("worker: lacks a hook but is unreachable: waits", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.addWorker(backup("nas", false, nil))
		sc := e.create(e.hookedInput("nightly", schedules.Target{WorkerID: w}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending || !e.schedule(sc.ID).Enabled {
			t.Fatalf("run = %+v, want it waiting", r)
		}
	})
}
