package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// backupKind stands in for the restic_backup kind: a job on the
// definition's repository that holds the repository's concurrency key and
// needs the restic.v2 worker feature.
type backupKind struct{}

func (backupKind) Name() string { return "restic_backup" }

func (backupKind) Check(def *syncrv1.JobDefinition) (jobkind.Refs, error) {
	id, err := domain.ParseResticRepoID(def.GetResticBackup().GetRepositoryId())
	if err != nil {
		return jobkind.Refs{}, errors.Join(domain.ErrInvalidArgument, err)
	}
	return jobkind.Refs{RepositoryID: id, SearchTerms: []string{"backup"}}, nil
}

func (backupKind) Build(*syncrv1.JobDefinition, jobkind.Resolved) (*syncrv1.JobSpec, error) {
	return &syncrv1.JobSpec{}, nil
}
func (backupKind) Describe(*syncrv1.JobDefinition, jobkind.Names) string { return "backup" }
func (backupKind) RequiredFeatures() []string                            { return []string{link.FeatureResticV2} }

// RequiredOS is the real backup's: Windows VSS needs a Windows worker.
func (backupKind) RequiredOS(def *syncrv1.JobDefinition) jobkind.OSRequirement {
	return jobkind.ResticBackup{}.RequiredOS(def)
}

func (k backupKind) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	refs, err := k.Check(def)
	if err != nil {
		return nil
	}
	return []string{jobkind.ResticRepoKey(refs.RepositoryID)}
}

// resticEnv is an env whose kinds include the backup stand-in, with one
// repository of alice in the store.
func resticEnv(t *testing.T) (*env, domain.ResticRepoID) {
	t.Helper()
	e := newEnv(t)
	var err error
	if e.kinds, err = jobkind.NewRegistry(jobkind.Transfer{}, backupKind{}); err != nil {
		t.Fatal(err)
	}
	remote := e.addRemote("Archive")
	repo := restic.Repo{
		ID: domain.NewResticRepoID(), OwnerID: e.alice.UserID, Name: "Photos vault", RemoteID: remote, Path: "photos",
		PasswordEnc: []byte("enc"), ResticID: strings.Repeat("ab", 32), FormatVersion: 2, CreatedAt: t0, UpdatedAt: t0, Version: 1,
	}
	if err := e.store.CreateRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	e.repos.set(repo.ID, remote, nil)
	return e, repo.ID
}

func (e *env) backupInput(name string, target schedules.Target, repo domain.ResticRepoID) schedules.Input {
	in := e.input(name, target)
	in.Kind = "restic_backup"
	in.Definition = fmt.Appendf(nil, `{"restic_backup":{"repository_id":%q,"paths":["/data"]}}`, repo)
	in.RepositoryID = repo
	return in
}

// TestRepositoryJobCarriesItsKeys: a scheduled restic job is created with
// its repository and the repository's concurrency key.
func TestRepositoryJobCarriesItsKeys(t *testing.T) {
	t.Parallel()
	e, repo := resticEnv(t)
	w := e.addWorker(worker{name: "nas", online: true, kinds: []string{"restic_backup"}, features: link.Features()})
	sc := e.create(e.backupInput("nightly", schedules.Target{WorkerID: w}, repo))
	e.clock.Set(t0.Add(time.Hour))
	e.tick(e.newScheduler(nil, nil))

	js := e.jobsOf(sc.ID)
	if len(js) != 1 {
		t.Fatalf("jobs = %+v, want one", js)
	}
	if j := js[0]; j.RepositoryID != repo || !slices.Equal(j.ConcurrencyKeys, []string{jobkind.ResticRepoKey(repo)}) || j.Kind != "restic_backup" {
		t.Errorf("job repository %s, keys %v, kind %s", j.RepositoryID, j.ConcurrencyKeys, j.Kind)
	}
	if r := e.onlyRun(sc.ID); r.Status != schedules.RunLaunched {
		t.Errorf("run = %+v", r)
	}
}

// TestRepositoryGoneOrDeleting: a launch on a repository that is gone, or
// whose data is being deleted, fails the run and pauses the schedule.
func TestRepositoryGoneOrDeleting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		state  error
		reason string
	}{
		{"gone", fmt.Errorf("repository: %w", domain.ErrNotFound), schedules.ReasonRepositoryUnavailable},
		{"deleting", fmt.Errorf("the repository's data is being deleted: %w", domain.ErrFailedPrecondition), schedules.ReasonRepositoryDeleting},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, repo := resticEnv(t)
			w := e.addWorker(worker{name: "nas", online: true, kinds: []string{"restic_backup"}, features: link.Features()})
			sc := e.create(e.backupInput("nightly", schedules.Target{WorkerID: w}, repo))
			e.repos.set(repo, domain.RemoteID{}, tt.state)
			e.clock.Set(t0.Add(time.Hour))
			s := e.newScheduler(nil, nil)
			e.tick(s)

			r := e.onlyRun(sc.ID)
			if r.Status != schedules.RunFailed || r.Reason != tt.reason || r.Message == "" {
				t.Fatalf("run = %+v, want failed with %s", r, tt.reason)
			}
			if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != tt.reason {
				t.Errorf("schedule enabled %v, paused reason %q", got.Enabled, got.PausedReason)
			}
			if js := e.jobsOf(sc.ID); len(js) != 0 {
				t.Errorf("jobs = %+v, want none", js)
			}
			// Nothing more happens on later ticks.
			e.clock.Advance(2 * time.Hour)
			e.tick(s)
			if rs := e.runs(sc.ID); len(rs) != 1 {
				t.Errorf("runs = %+v, want the failed one only", rs)
			}
		})
	}
}

// TestLabelSkipsOutdatedWorkers: a label's worker that lacks a feature the
// kind requires cannot run it, although it lists the kind.
func TestLabelSkipsOutdatedWorkers(t *testing.T) {
	t.Parallel()
	e, repo := resticEnv(t)
	// The outdated worker is idle and first by name; the current one busy.
	e.addWorker(worker{name: "a-old", online: true, labels: []string{"nas"}, kinds: []string{"restic_backup"}})
	current := e.addWorker(worker{name: "b-current", online: true, labels: []string{"nas"}, kinds: []string{"restic_backup"}, features: link.Features()})
	e.busyJob(current)
	sc := e.create(e.backupInput("nightly", schedules.Target{Label: "nas"}, repo))
	e.clock.Set(t0.Add(time.Hour))
	e.tick(e.newScheduler(nil, nil))
	if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != current {
		t.Fatalf("jobs = %+v, want one on the current worker", js)
	}

	// Only outdated workers: the run fails, the schedule stays enabled.
	e2, repo2 := resticEnv(t)
	e2.addWorker(worker{name: "old", online: true, labels: []string{"nas"}, kinds: []string{"restic_backup"}})
	sc2 := e2.create(e2.backupInput("nightly", schedules.Target{Label: "nas"}, repo2))
	e2.clock.Set(t0.Add(time.Hour))
	e2.tick(e2.newScheduler(nil, nil))
	r := e2.onlyRun(sc2.ID)
	if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonNoWorkerWithLabel || !e2.schedule(sc2.ID).Enabled {
		t.Fatalf("run = %+v", r)
	}

	// Transfers need no feature: old workers still take them.
	e3 := newEnv(t)
	old := e3.addWorker(worker{name: "old", online: true, labels: []string{"nas"}})
	sc3 := e3.create(e3.input("copy", schedules.Target{Label: "nas"}))
	e3.clock.Set(t0.Add(time.Hour))
	e3.tick(e3.newScheduler(nil, nil))
	if js := e3.jobsOf(sc3.ID); len(js) != 1 || js[0].WorkerID != old {
		t.Fatalf("transfer jobs = %+v", js)
	}
}

// TestPinnedOutdatedWorker: a schedule pinned to a worker that reports it
// cannot run the kind (it lacks the kind's link feature, an older version,
// or does not list the kind) fails the run without creating a job and
// pauses the schedule, once the worker is reachable; an unreachable one
// keeps the run waiting, since its report may be a previous process's.
func TestPinnedOutdatedWorker(t *testing.T) {
	t.Parallel()
	for name, w := range map[string]worker{
		"lacks the feature":    {name: "old", online: true, kinds: []string{"restic_backup"}},
		"does not list a kind": {name: "old", online: true, kinds: []string{"rclone_transfer"}, features: link.Features()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, repo := resticEnv(t)
			id := e.addWorker(w)
			sc := e.create(e.backupInput("nightly", schedules.Target{WorkerID: id}, repo))
			e.clock.Set(t0.Add(time.Hour))
			e.tick(e.newScheduler(nil, nil))
			r := e.onlyRun(sc.ID)
			if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonWorkerOutdated ||
				!strings.HasPrefix(r.Message, `the schedule's worker "old" (version `) || !strings.Contains(r.Message, "cannot run restic_backup jobs") {
				t.Fatalf("run = %+v", r)
			}
			if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonWorkerOutdated || len(e.jobsOf(sc.ID)) != 0 {
				t.Errorf("schedule enabled %v, paused reason %q, jobs %d", got.Enabled, got.PausedReason, len(e.jobsOf(sc.ID)))
			}
		})
	}

	t.Run("unreachable: waits", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		id := e.addWorker(worker{name: "old", online: false, kinds: []string{"restic_backup"}})
		sc := e.create(e.backupInput("nightly", schedules.Target{WorkerID: id}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending || !e.schedule(sc.ID).Enabled {
			t.Fatalf("run = %+v, want it waiting", r)
		}
	})
}

// vssInput is backupInput with Windows VSS on.
func (e *env) vssInput(name string, target schedules.Target, repo domain.ResticRepoID) schedules.Input {
	in := e.backupInput(name, target, repo)
	in.Definition = fmt.Appendf(nil, `{"restic_backup":{"repository_id":%q,"paths":["C:\\Users"],"use_fs_snapshot":true}}`, repo)
	return in
}

// TestTargetsMeetTheOS: a definition that needs an operating system (Windows
// VSS) only goes to workers that run it. A label's other workers are
// skipped; a label without one fails the run (the schedule stays enabled);
// a chosen worker on another system fails it and pauses the schedule. A
// worker that has not reported its system yet may still take it.
func TestTargetsMeetTheOS(t *testing.T) {
	t.Parallel()
	backup := func(name, os string, online bool, labels ...string) worker {
		return worker{name: name, online: online, labels: labels, kinds: []string{"restic_backup"}, features: link.Features(), os: os}
	}

	t.Run("label: only the Windows worker", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-linux", "linux", true, "office")) // idle and first by name
		win := e.addWorker(backup("b-windows", "windows", true, "office"))
		e.busyJob(win)
		sc := e.create(e.vssInput("documents", schedules.Target{Label: "office"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != win {
			t.Fatalf("jobs = %+v, want one on the Windows worker", js)
		}
	})

	t.Run("label: no Windows worker", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-linux", "linux", true, "office"))
		e.addWorker(backup("b-linux", "linux", false, "office"))
		sc := e.create(e.vssInput("documents", schedules.Target{Label: "office"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		r := e.onlyRun(sc.ID)
		if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonNoWorkerWithLabel ||
			r.Message != `no worker with the label "office" can run this job: Windows VSS (use_fs_snapshot) is only available on Windows workers` {
			t.Fatalf("run = %+v", r)
		}
		if !e.schedule(sc.ID).Enabled || len(e.jobsOf(sc.ID)) != 0 {
			t.Errorf("schedule enabled %v, jobs %d", e.schedule(sc.ID).Enabled, len(e.jobsOf(sc.ID)))
		}
	})

	t.Run("label: a worker that has not reported its system may", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		e.addWorker(backup("a-linux", "linux", true, "office"))
		e.addWorker(worker{name: "new", online: false, labels: []string{"office"}, kinds: []string{}}) // never reported
		sc := e.create(e.vssInput("documents", schedules.Target{Label: "office"}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		if r := e.onlyRun(sc.ID); r.Status != schedules.RunPending {
			t.Fatalf("run = %+v, want it waiting", r)
		}
	})

	t.Run("worker: another system", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.addWorker(backup("nas", "linux", true))
		sc := e.create(e.vssInput("documents", schedules.Target{WorkerID: w}, repo))
		e.clock.Set(t0.Add(time.Hour))
		e.tick(e.newScheduler(nil, nil))
		r := e.onlyRun(sc.ID)
		if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonInvalidDefinition ||
			!strings.HasPrefix(r.Message, `Windows VSS (use_fs_snapshot) is only available on Windows workers; the schedule's worker "nas" runs linux.`) {
			t.Fatalf("run = %+v", r)
		}
		if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonInvalidDefinition || len(e.jobsOf(sc.ID)) != 0 {
			t.Errorf("schedule enabled %v, paused reason %q, jobs %d", got.Enabled, got.PausedReason, len(e.jobsOf(sc.ID)))
		}
	})

	t.Run("worker: Windows, or not reported", func(t *testing.T) {
		t.Parallel()
		for _, os := range []string{"windows", ""} {
			e, repo := resticEnv(t)
			w := e.addWorker(backup("pc", os, true))
			sc := e.create(e.vssInput("documents", schedules.Target{WorkerID: w}, repo))
			e.clock.Set(t0.Add(time.Hour))
			e.tick(e.newScheduler(nil, nil))
			if js := e.jobsOf(sc.ID); len(js) != 1 || js[0].WorkerID != w {
				t.Errorf("os %q: jobs = %+v, want one on the worker", os, js)
			}
		}
	})
}
