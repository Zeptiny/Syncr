package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
		def.GetResticDeleteData().GetRepositoryId() != repo.String() || def.GetResticDeleteData().GetResume() || !p.ScheduleID.IsZero() {
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

// A deletion resumes (may take an empty location for done) only after an
// earlier deletion job of the repository ran on the same worker and deleted
// something there. Anything else may have looked at the wrong place.
func TestDataDeleterResumesOnlyAfterADeletionHere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, workerA, workerB := domain.NewResticRepoID(), domain.NewWorkerID(), domain.NewWorkerID()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	// attempt is an earlier deletion job on worker w that ended in st, ran
	// (a worker accepted it) or not, and deleted that many objects by its
	// final stats.
	attempt := func(w domain.WorkerID, st domain.JobStatus, ran bool, deletes int64, code string) jobs.Job {
		j := jobs.Job{ID: domain.NewJobID(), WorkerID: w, Kind: "restic_delete_data", RepositoryID: repo, Status: st,
			Stats: jobs.Stats{Deletes: deletes}, Error: jobs.JobError{Code: code}}
		if ran {
			j.DispatchedAt, j.StartedAt = at, at
		}
		return j
	}
	notFoundOnA := attempt(workerA, domain.JobFailed, true, 0, "repository_not_found")
	deletedOnA := attempt(workerA, domain.JobCancelled, true, 42, "cancelled")
	lostWithProgress := attempt(workerA, domain.JobInterrupted, true, 0, "worker_lost")
	lostWithoutProgress := attempt(workerA, domain.JobInterrupted, true, 0, "worker_lost")
	tests := []struct {
		name    string
		earlier []jobs.Job
		samples map[domain.JobID]jobs.Sample
		worker  domain.WorkerID
		want    bool
	}{
		{"a first attempt", nil, nil, workerA, false},
		// (1) The wrong worker found nothing; asking it again must not
		// take that for done.
		{"a retry after nothing was found on the same worker", []jobs.Job{notFoundOnA}, nil, workerA, false},
		// (2) and (3): files were deleted on worker A.
		{"a retry on another worker after a deletion on A", []jobs.Job{deletedOnA}, nil, workerB, false},
		{"a retry on A after a deletion on A", []jobs.Job{deletedOnA}, nil, workerA, true},
		{"a retry on A after a deletion on A and a failure since", []jobs.Job{deletedOnA, notFoundOnA}, nil, workerA, true},
		// (4) Cancelled, or failed, before deleting anything.
		{"after a cancelled attempt that deleted nothing", []jobs.Job{attempt(workerA, domain.JobCancelled, true, 0, "cancelled")}, nil, workerA, false},
		{"after a wrong password", []jobs.Job{attempt(workerA, domain.JobFailed, true, 0, "wrong_password")}, nil, workerA, false},
		{"after a mismatch", []jobs.Job{attempt(workerA, domain.JobFailed, true, 0, "repository_mismatch")}, nil, workerA, false},
		{"after a locked repository", []jobs.Job{attempt(workerA, domain.JobFailed, true, 0, "repository_locked")}, nil, workerA, false},
		{"after attempts that never left the queue", []jobs.Job{attempt(workerA, domain.JobFailed, false, 0, "dispatch_timeout")}, nil, workerA, false},
		// A job lost without a final result counts by its progress.
		{"after a lost attempt whose progress shows deletions", []jobs.Job{lostWithProgress},
			map[domain.JobID]jobs.Sample{lostWithProgress.ID: {JobID: lostWithProgress.ID, Stats: jobs.Stats{Deletes: 7}}}, workerA, true},
		{"after a lost attempt without progress", []jobs.Job{lostWithoutProgress},
			map[domain.JobID]jobs.Sample{lostWithoutProgress.ID: {JobID: lostWithoutProgress.ID}}, workerA, false},
		{"on another worker after a lost attempt with progress on A", []jobs.Job{lostWithProgress},
			map[domain.JobID]jobs.Sample{lostWithProgress.ID: {JobID: lostWithProgress.ID, Stats: jobs.Stats{Deletes: 7}}}, workerB, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			d := newDeleter(t, e, deleteKind{})
			job := domain.NewJobID()
			// The job itself, when it already exists (a repeated Start),
			// does not count; newest first, one per page.
			list := append([]jobs.Job{{ID: job, WorkerID: tt.worker, Kind: "restic_delete_data", RepositoryID: repo, Status: domain.JobRunning,
				DispatchedAt: at, Stats: jobs.Stats{Deletes: 99}}}, tt.earlier...)
			slices.SortFunc(list, func(a, b jobs.Job) int { return strings.Compare(b.ID.String(), a.ID.String()) })
			e.jobs.deletions = map[domain.ResticRepoID][]jobs.Job{repo: list}
			e.jobs.samples = tt.samples
			e.jobs.pageSize = 1
			if err := d.Start(ctx, alice.UserID, repo, job, tt.worker); err != nil {
				t.Fatal(err)
			}
			def, err := jobkind.Unmarshal(e.jobs.created[0].Definition)
			if err != nil {
				t.Fatal(err)
			}
			if got := def.GetResticDeleteData().GetResume(); got != tt.want {
				t.Errorf("resume = %v, want %v", got, tt.want)
			}
			// Samples are read only for attempts on this worker without a
			// final count, in one query.
			for _, ask := range e.jobs.sampleAsks {
				if slices.Contains(ask, job) || slices.Contains(ask, deletedOnA.ID) {
					t.Errorf("samples asked for %v", ask)
				}
			}
			if len(e.jobs.sampleAsks) > 1 {
				t.Errorf("samples asked %d times", len(e.jobs.sampleAsks))
			}
		})
	}

	// Without the history nothing is queued: a retry decides again.
	for name, mutate := range map[string]func(*fakeJobs){
		"jobs":    func(f *fakeJobs) { f.listErr = errors.New("db down") },
		"samples": func(f *fakeJobs) { f.sampleErr = errors.New("db down") },
	} {
		e := newEnv(t)
		d := newDeleter(t, e, deleteKind{})
		e.jobs.deletions = map[domain.ResticRepoID][]jobs.Job{repo: {lostWithProgress}}
		mutate(e.jobs)
		err := d.Start(ctx, alice.UserID, repo, domain.NewJobID(), workerA)
		if err == nil || errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrFailedPrecondition) || len(e.jobs.created) != 0 {
			t.Errorf("Start without the %s = %v (%d created), want a transient error", name, err, len(e.jobs.created))
		}
	}
}
