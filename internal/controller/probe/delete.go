package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// deleteDataKind is the job kind that deletes a repository's data.
const deleteDataKind = "restic_delete_data"

// DeleterJobs is the part of the job use cases the DataDeleter uses
// (*jobs.Service, or cmd's late-bound adapter of it).
type DeleterJobs interface {
	// Create queues a job for the actor; with a preset p.ID it fails with
	// ErrAlreadyExists once that job exists.
	Create(ctx context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error)
	Jobs
}

// DeleterRemotes resolves the repository's remote (*remotes.Service).
type DeleterRemotes interface {
	Resolve(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Resolved, error)
}

// DeleterDeps are a DataDeleter's collaborators. All are required except
// Logger.
type DeleterDeps struct {
	Jobs    DeleterJobs
	Workers Workers
	Remotes DeleterRemotes
	// Kinds must hold the restic_delete_data kind.
	Kinds *jobkind.Registry
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// DataDeleter starts the restic_delete_data job of a repository whose
// owner asked to delete its data too (M5 design §1.7). It implements
// restic.Deletions. Create it with NewDataDeleter; it holds no state and
// is safe for concurrent use.
type DataDeleter struct {
	jobs    DeleterJobs
	workers Workers
	remotes DeleterRemotes
	kinds   *jobkind.Registry
	log     *slog.Logger
}

var _ restic.Deletions = (*DataDeleter)(nil)

// NewDataDeleter returns a DataDeleter.
func NewDataDeleter(d DeleterDeps) (*DataDeleter, error) {
	if d.Jobs == nil || d.Workers == nil || d.Remotes == nil || d.Kinds == nil {
		return nil, errors.New("probe: data deleter: missing dependency")
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &DataDeleter{jobs: d.Jobs, workers: d.Workers, remotes: d.Remotes, kinds: d.Kinds, log: log}, nil
}

// Pick implements restic.Deletions: it chooses the worker of the deletion
// job without changing anything. workerID, if set, must be online and able
// to run the kind. Otherwise a remote on a worker's local filesystem needs
// one (*domain.ValidationError on "worker_id"), and any other remote gets
// an online worker that runs the kind with its features: the worker of the
// owner's newest succeeded job on the remote first (its restic cache is
// warm), then the least busy one.
//
// Errors: domain.ErrNotFound (the remote or the worker),
// domain.ErrUnavailable (the chosen worker is offline),
// domain.ErrFailedPrecondition (no usable worker, or an outdated one).
func (d *DataDeleter) Pick(ctx context.Context, owner domain.UserID, remoteID domain.RemoteID, workerID domain.WorkerID) (domain.WorkerID, error) {
	const what = "choose the worker that deletes the data"
	rs, err := d.remotes.Resolve(ctx, owner, []domain.RemoteID{remoteID})
	if err != nil {
		return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
	}
	r, ok := rs[remoteID]
	if !ok {
		return domain.WorkerID{}, fmt.Errorf("%s: remote %w", what, domain.ErrNotFound)
	}
	required := d.kinds.RequiredFeatures(deleteDataKind)
	if !workerID.IsZero() {
		w, err := d.workers.Get(ctx, workerID)
		found := err == nil && !w.Removed()
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
		}
		if err := CheckExplicitForJob(candidateOf(w, d.workers.Status(w)), found, deleteDataKind, required); err != nil {
			return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
		}
		return w.ID, nil
	}
	if r.Rclone.Backend == "" {
		return domain.WorkerID{}, fmt.Errorf("%s: %w", what, fieldError("worker_id", "choose the worker whose disk holds this repository"))
	}
	ws, err := d.workers.List(ctx, workers.ListFilter{})
	if err != nil {
		return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
	}
	preferred, err := lastWorker(ctx, d.jobs, owner, jobs.ListFilter{RemoteID: remoteID}, true)
	if err != nil {
		return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
	}
	cands := make([]Candidate, 0, len(ws))
	for _, w := range ws {
		if w.Removed() {
			continue
		}
		c := candidateOf(w, d.workers.Status(w))
		c.Preferred = !preferred.IsZero() && w.ID == preferred
		if c.Status == domain.WorkerOnline && c.canRun(deleteDataKind, required) {
			js, err := d.jobs.Unfinished(ctx, w.ID)
			if err != nil {
				return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
			}
			c.Unfinished = len(js)
		}
		cands = append(cands, c)
	}
	c, err := PickForJob(cands, deleteDataKind, required)
	if err != nil {
		return domain.WorkerID{}, fmt.Errorf("%s: %w", what, err)
	}
	return c.ID, nil
}

// Start implements restic.Deletions: it queues the restic_delete_data job
// of repository repoID for owner on workerID, with the preset jobID (the
// repository's reserved deletion job ID, which is what lets the job
// through while the repository is deleting). A job that already exists
// counts as success, so Start may be repeated.
//
// Permanent errors match domain.ErrNotFound (the worker, the repository
// or its remote is gone) or domain.ErrFailedPrecondition (the controller
// cannot build the job); anything else is transient and may be retried.
func (d *DataDeleter) Start(ctx context.Context, owner domain.UserID, repoID domain.ResticRepoID, jobID domain.JobID, workerID domain.WorkerID) error {
	what := fmt.Sprintf("start deleting the data of repository %s", repoID)
	if jobID.IsZero() || workerID.IsZero() || owner.IsZero() || repoID.IsZero() {
		return fmt.Errorf("%s: %w", what, domain.ErrInvalidID)
	}
	def := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{
		ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repoID.String()},
	}}
	kind, refs, err := d.kinds.Check(def)
	if err != nil {
		return fmt.Errorf("%s: %w: %w", what, domain.ErrFailedPrecondition, err)
	}
	raw, err := jobkind.Marshal(def)
	if err != nil {
		return fmt.Errorf("%s: %w: %w", what, domain.ErrFailedPrecondition, err)
	}
	_, err = d.jobs.Create(ctx, domain.Actor{UserID: owner, Role: domain.RoleUser}, jobs.CreateParams{
		ID: jobID, WorkerID: workerID, Kind: kind, Definition: raw,
		RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
		RepositoryID: repoID, ConcurrencyKeys: refs.ConcurrencyKeys,
	})
	switch {
	case err == nil:
		d.log.InfoContext(ctx, "repository data deletion queued", "repository_id", repoID.String(),
			"job_id", jobID.String(), "worker_id", workerID.String())
		return nil
	case errors.Is(err, domain.ErrAlreadyExists):
		return nil // an earlier Start created it
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidID):
		return fmt.Errorf("%s: %w: %w", what, domain.ErrFailedPrecondition, err)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}
