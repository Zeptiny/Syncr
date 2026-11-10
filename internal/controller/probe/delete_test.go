package probe_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// deleteKind stands in for the restic_delete_data kind.
type deleteKind struct{}

func (deleteKind) Name() string { return "restic_delete_data" }

func (deleteKind) Check(def *syncrv1.JobDefinition) (jobkind.Refs, error) {
	id, err := domain.ParseResticRepoID(def.GetResticDeleteData().GetRepositoryId())
	if err != nil {
		return jobkind.Refs{}, errors.Join(domain.ErrInvalidArgument, err)
	}
	return jobkind.Refs{RepositoryID: id, SearchTerms: []string{"delete data"}}, nil
}

func (deleteKind) Build(*syncrv1.JobDefinition, jobkind.Resolved) (*syncrv1.JobSpec, error) {
	return &syncrv1.JobSpec{}, nil
}
func (deleteKind) Describe(*syncrv1.JobDefinition, jobkind.Names) string { return "delete" }
func (deleteKind) SystemOnly() string                                    { return "is started by deleting the repository" }
func (deleteKind) RequiredFeatures() []string {
	return []string{link.FeatureResticV2, link.FeatureResticDelete}
}

func (k deleteKind) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	refs, err := k.Check(def)
	if err != nil {
		return nil
	}
	return []string{jobkind.ResticRepoKey(refs.RepositoryID)}
}

func newDeleter(t *testing.T, e *env, kinds ...jobkind.Kind) *probe.DataDeleter {
	t.Helper()
	reg, err := jobkind.NewRegistry(kinds...)
	if err != nil {
		t.Fatal(err)
	}
	d, err := probe.NewDataDeleter(probe.DeleterDeps{Jobs: e.jobs, Workers: e.workers, Remotes: e.remotes, Kinds: reg})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDataDeleterPick(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := newDeleter(t, e, jobkind.Transfer{}, deleteKind{})
	remote, disk := e.remotes.s3("bucket"), e.remotes.local("/srv/backups")
	ctx := context.Background()

	if _, err := d.Pick(ctx, alice.UserID, remote, domain.WorkerID{}); !errors.Is(err, probe.ErrNoWorker) {
		t.Fatalf("no workers: %v", err)
	}
	busy := e.workers.add("busy", domain.WorkerOnline, current())
	idle := e.workers.add("idle", domain.WorkerOnline, current())
	noDelete := current()
	noDelete.Features = []string{link.FeatureProbe, link.FeatureResticV2}
	partial := e.workers.add("a-partial", domain.WorkerOnline, noDelete)
	away := e.workers.add("away", domain.WorkerOffline, current())
	e.workers.add("old", domain.WorkerOnline, workers.Info{JobKinds: []string{"restic_delete_data"}})
	e.jobs.unfinished[busy] = 2

	// The least busy worker that has the kind and both features.
	if got, err := d.Pick(ctx, alice.UserID, remote, domain.WorkerID{}); err != nil || got != idle {
		t.Fatalf("picked %v, %v; want idle", got, err)
	}
	// The worker that last succeeded on the remote is preferred.
	e.jobs.lastByRem[remote] = busy
	if got, err := d.Pick(ctx, alice.UserID, remote, domain.WorkerID{}); err != nil || got != busy {
		t.Fatalf("picked %v, %v; want the preferred worker", got, err)
	}

	var verr *domain.ValidationError
	tests := []struct {
		name   string
		owner  domain.UserID
		remote domain.RemoteID
		worker domain.WorkerID
		want   error
	}{
		{"another user's remote", bob.UserID, remote, domain.WorkerID{}, domain.ErrNotFound},
		{"an unknown worker", alice.UserID, remote, domain.NewWorkerID(), domain.ErrNotFound},
		{"an offline worker", alice.UserID, remote, away, domain.ErrUnavailable},
		{"a worker without the delete feature", alice.UserID, remote, partial, domain.ErrFailedPrecondition},
		{"a local remote without a worker", alice.UserID, disk, domain.WorkerID{}, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		if _, err := d.Pick(ctx, tt.owner, tt.remote, tt.worker); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	if _, err := d.Pick(ctx, alice.UserID, disk, domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "worker_id" {
		t.Errorf("local remote: %v", err)
	}
	if got, err := d.Pick(ctx, alice.UserID, disk, idle); err != nil || got != idle {
		t.Errorf("local remote on a chosen worker: %v, %v", got, err)
	}
	if len(e.jobs.created) != 0 {
		t.Error("Pick created a job")
	}
}

func TestDataDeleterStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := newDeleter(t, e, jobkind.Transfer{}, deleteKind{})
	ctx := context.Background()
	repo, job, worker := domain.NewResticRepoID(), domain.NewJobID(), domain.NewWorkerID()

	if err := d.Start(ctx, alice.UserID, repo, job, worker); err != nil {
		t.Fatal(err)
	}
	if len(e.jobs.created) != 1 {
		t.Fatalf("created %d jobs", len(e.jobs.created))
	}
	p := e.jobs.created[0]
	def, err := jobkind.Unmarshal(p.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != job || p.WorkerID != worker || p.Kind != "restic_delete_data" || p.RepositoryID != repo ||
		!slices.Equal(p.ConcurrencyKeys, []string{jobkind.ResticRepoKey(repo)}) || !slices.Equal(p.SearchTerms, []string{"delete data"}) ||
		def.GetResticDeleteData().GetRepositoryId() != repo.String() || !p.ScheduleID.IsZero() {
		t.Errorf("created %+v", p)
	}

	// Repeating it is fine: the job exists.
	e.jobs.createErr = errors.Join(domain.ErrAlreadyExists, errors.New("job exists"))
	if err := d.Start(ctx, alice.UserID, repo, job, worker); err != nil {
		t.Errorf("repeated Start: %v", err)
	}

	tests := []struct {
		name      string
		createErr error
		permanent bool
		want      error
	}{
		{"the worker was removed", jobs.ErrUnknownWorker, true, domain.ErrNotFound},
		{"the repository is gone", jobs.ErrUnknownRepository, true, domain.ErrNotFound},
		{"the repository is not this deletion's", jobs.ErrRepositoryDeleting, true, domain.ErrFailedPrecondition},
		{"the job is invalid", errors.Join(domain.ErrInvalidArgument, errors.New("keys")), true, domain.ErrFailedPrecondition},
		{"the store is down", errors.New("db down"), false, nil},
	}
	for _, tt := range tests {
		e.jobs.createErr = tt.createErr
		err := d.Start(ctx, alice.UserID, repo, job, worker)
		if err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		permanent := errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrFailedPrecondition)
		if permanent != tt.permanent || (tt.want != nil && !errors.Is(err, tt.want)) {
			t.Errorf("%s: err = %v (permanent %v), want %v (permanent %v)", tt.name, err, permanent, tt.want, tt.permanent)
		}
	}
	e.jobs.createErr = nil

	if err := d.Start(ctx, alice.UserID, repo, domain.JobID{}, worker); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("no job ID: %v", err)
	}
	// A controller without the kind cannot delete data: a permanent error.
	bare := newDeleter(t, e, jobkind.Transfer{})
	if err := bare.Start(ctx, alice.UserID, repo, job, worker); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("without the kind: %v", err)
	}
	if _, err := probe.NewDataDeleter(probe.DeleterDeps{}); err == nil {
		t.Error("missing dependencies accepted")
	}
}
