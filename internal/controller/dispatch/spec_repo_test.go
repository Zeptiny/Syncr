package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// backupKind stands in for the restic_backup kind: it refers to the
// definition's repository, requires the restic.v2 feature and builds a
// backup spec from the resolved repository.
type backupKind struct{}

func (backupKind) Name() string { return "restic_backup" }

func (backupKind) Check(def *syncrv1.JobDefinition) (jobkind.Refs, error) {
	id, err := domain.ParseResticRepoID(def.GetResticBackup().GetRepositoryId())
	if err != nil {
		return jobkind.Refs{}, errors.Join(domain.ErrInvalidArgument, err)
	}
	return jobkind.Refs{RepositoryID: id}, nil
}

func (k backupKind) Build(def *syncrv1.JobDefinition, in jobkind.Resolved) (*syncrv1.JobSpec, error) {
	refs, err := k.Check(def)
	if err != nil {
		return nil, err
	}
	repo, ok := in.Repositories[refs.RepositoryID]
	if !ok {
		return nil, errors.Join(domain.ErrNotFound, errors.New("the repository was not resolved"))
	}
	defs, rr, err := jobkind.LocateRepository(repo, in.Remotes)
	if err != nil {
		return nil, err
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
		Repository: rr, Paths: def.GetResticBackup().GetPaths(), Tags: []string{repo.ResticID},
	}}}, nil
}

func (backupKind) Describe(*syncrv1.JobDefinition, jobkind.Names) string { return "backup" }
func (backupKind) RequiredFeatures() []string                            { return []string{link.FeatureResticV2} }

// fakeRepos resolves one repository of testOwner.
type fakeRepos struct {
	repo restic.Resolved
	err  error
}

func (f *fakeRepos) Resolve(_ context.Context, owner domain.UserID, id domain.ResticRepoID) (restic.Resolved, error) {
	if f.err != nil {
		return restic.Resolved{}, f.err
	}
	if owner != f.repo.OwnerID || id != f.repo.ID {
		return restic.Resolved{}, restic.ErrUnknownRepository
	}
	return f.repo, nil
}

const testRepoPassword = "repo-password-sentinel"

func repoSpecs(t *testing.T, repos dispatch.Repos) (*dispatch.Specs, *fakeRemotes) {
	t.Helper()
	reg, err := jobkind.NewRegistry(jobkind.Transfer{}, backupKind{})
	if err != nil {
		t.Fatal(err)
	}
	rem := newFakeRemotes()
	specs, err := dispatch.NewSpecs(rem, repos, reg)
	if err != nil {
		t.Fatal(err)
	}
	return specs, rem
}

func backupJob(t *testing.T, repo domain.ResticRepoID) jobs.Job {
	t.Helper()
	raw, err := jobkind.Marshal(&syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{
		ResticBackup: &syncrv1.ResticBackupDefinition{RepositoryId: repo.String(), Paths: []string{"/data/photos"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: domain.NewJobID(), OwnerID: testOwner, Kind: "restic_backup", Definition: raw}
}

func TestSpecsResolveTheRepository(t *testing.T) {
	t.Parallel()
	repoID := domain.NewResticRepoID()
	repos := &fakeRepos{repo: restic.Resolved{
		ID: repoID, OwnerID: testOwner, Name: "Photos", RemoteID: testRemote(), Path: "backups/photos",
		Password: testRepoPassword, ResticID: strings.Repeat("ab", 32),
	}}
	specs, rem := repoSpecs(t, repos)
	current := workers.Info{Features: link.Features()}

	spec, err := specs.Build(context.Background(), backupJob(t, repoID), current)
	if err != nil {
		t.Fatal(err)
	}
	b := spec.GetResticBackup()
	if b.GetRepository().GetPassword() != testRepoPassword || b.GetRepository().GetLocation().GetPath() != "bucket/backups/photos" ||
		b.GetRepository().GetLocation().GetRemote() == "" || b.GetTags()[0] != repos.repo.ResticID {
		t.Errorf("repository = %+v, tags %v", b.GetRepository().GetLocation(), b.GetTags())
	}
	// The repository's remote was resolved although the definition names
	// no remote, with its secret.
	if len(spec.GetRemotes()) != 1 || spec.GetRemotes()[0].GetSecrets()["secret_access_key"] != testSecret || rem.calls() != 1 {
		t.Errorf("remotes = %d, resolve calls %d", len(spec.GetRemotes()), rem.calls())
	}

	tests := []struct {
		name  string
		setup func()
		job   jobs.Job
		info  workers.Info
		is    []error
		not   []error
	}{
		{"a worker older than Milestone 5", nil, backupJob(t, repoID), workers.Info{JobKinds: []string{"restic_backup"}},
			[]error{dispatch.ErrWorkerOutdated}, []error{domain.ErrNotFound}},
		{"a worker with other features only", nil, backupJob(t, repoID), workers.Info{Features: []string{link.FeatureProbe}},
			[]error{dispatch.ErrWorkerOutdated}, nil},
		{"an unknown repository", nil, backupJob(t, domain.NewResticRepoID()), current,
			[]error{domain.ErrNotFound}, []error{dispatch.ErrWorkerOutdated}},
		{"a password that cannot be decrypted", func() { repos.err = errors.Join(domain.ErrFailedPrecondition, errors.New("decrypt")) },
			backupJob(t, repoID), current, []error{domain.ErrFailedPrecondition}, []error{domain.ErrNotFound, dispatch.ErrWorkerOutdated}},
		{"a store failure", func() { repos.err = errors.New("db down") }, backupJob(t, repoID), current,
			nil, []error{domain.ErrNotFound, domain.ErrFailedPrecondition, domain.ErrInvalidArgument}},
		{"the repository's remote is gone", func() { repos.err = nil; rem.remove(testRemote()) }, backupJob(t, repoID), current,
			[]error{domain.ErrNotFound}, nil},
	}
	for _, tt := range tests {
		if tt.setup != nil {
			tt.setup()
		}
		_, err := specs.Build(context.Background(), tt.job, tt.info)
		if err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		for _, want := range tt.is {
			if !errors.Is(err, want) {
				t.Errorf("%s: err = %v, want it to match %v", tt.name, err, want)
			}
		}
		for _, not := range tt.not {
			if errors.Is(err, not) {
				t.Errorf("%s: err = %v must not match %v", tt.name, err, not)
			}
		}
		if strings.Contains(err.Error(), testRepoPassword) || strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: the error leaks a secret: %v", tt.name, err)
		}
	}

	// Without a repository service, repository jobs cannot be built.
	none, _ := repoSpecs(t, nil)
	if _, err := none.Build(context.Background(), backupJob(t, repoID), current); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no repos: err = %v", err)
	}
}

// TestRepositoryJobFailures: a job on an outdated worker fails with
// worker_outdated and one whose repository is gone with
// repository_unavailable, instead of being sent.
func TestRepositoryJobFailures(t *testing.T) {
	t.Parallel()
	repoID := domain.NewResticRepoID()
	repos := &fakeRepos{repo: restic.Resolved{ID: repoID, OwnerID: testOwner, RemoteID: testRemote(), Password: testRepoPassword}}

	e := newEnv(t)
	specs, _ := repoSpecs(t, repos)
	disp, err := dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: specs, Clock: e.clock, Hub: e.hub}, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.disp = disp
	old, current := e.newFakeWorker(e.enroll("old")), e.newProbeWorker(e.enroll("current"))
	old.report()
	current.report()

	queue := func(w domain.WorkerID, repo domain.ResticRepoID) jobs.Job {
		t.Helper()
		j, err := e.jobs.Create(context.Background(), testActor(), jobs.CreateParams{
			WorkerID: w, Kind: "restic_backup", Definition: backupJob(t, repo).Definition,
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	outdated := queue(old.id, repoID)
	gone := queue(current.id, domain.NewResticRepoID())
	fine := queue(current.id, repoID)

	if got := started(old.poll()); len(got) != 0 {
		t.Errorf("the old worker was sent %v", got)
	}
	if got := e.job(outdated.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated ||
		got.Error.Message != `worker "old" must be upgraded to run restic_backup jobs` {
		t.Errorf("outdated = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}
	if got := started(current.poll()); len(got) != 1 || got[0] != fine.ID.String() {
		t.Errorf("the current worker was sent %v, want %v", got, fine.ID)
	}
	if got := e.job(gone.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeRepositoryUnavailable {
		t.Errorf("gone = %s %q", got.Status, got.Error.Code)
	}
	if strings.Contains(e.logs.String(), testRepoPassword) || strings.Contains(e.logs.String(), testSecret) {
		t.Error("a secret was logged")
	}
}
