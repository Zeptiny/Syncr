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
