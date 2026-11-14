package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ResticService is the part of the restic use cases the API calls
// (implemented by *restic.Service; see internal/app/restic). Every use case
// taking an actor scopes by it: the actor's own repositories only, admins
// included, and domain.ErrNotFound otherwise. The repository password is
// write-only: no method returns it.
type ResticService interface {
	// Create initializes or connects to the repository on a worker, then
	// stores it; the result names the worker.
	Create(ctx context.Context, actor domain.Actor, in restic.Input) (restic.Created, error)
	Get(ctx context.Context, actor domain.Actor, id domain.ResticRepoID) (restic.Repo, error)
	List(ctx context.Context, actor domain.Actor, f restic.ListFilter) ([]restic.Repo, error)
	// Update renames the repository or replaces its password (verified on
	// a worker first); domain.ErrConflict on a stale version.
	Update(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, version int64, u restic.Update) (restic.Repo, error)
	// Delete forgets the repository or, with DeleteOptions.DeleteData,
	// starts the job that deletes its data first.
	Delete(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, o restic.DeleteOptions) (restic.DeleteResult, error)
	// Snapshots returns the cached snapshot list, refreshed through a
	// worker when asked or never listed.
	Snapshots(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, o restic.SnapshotsOptions) (restic.SnapshotList, error)
	// Browse lists one directory of a snapshot.
	Browse(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, snapshotID, path string, workerID domain.WorkerID) (restic.Listing, error)
	// Measure measures a folder of a snapshot: its totals and each child
	// folder's (partial at the scan timeout).
	Measure(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, snapshotID, path string, workerID domain.WorkerID) (restic.Measurement, error)
	// Find searches the repository's snapshots (restic find).
	Find(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, q restic.FindQuery, workerID domain.WorkerID) (restic.FindResult, error)
	// Compare compares snapshot fromID with the newer toID, the whole tree
	// or the folder path (restic diff).
	Compare(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, fromID, toID, path string, workerID domain.WorkerID) (restic.Comparison, error)
	RepositoryNames
	RemoteSourceChecker
}

// RemoteSourceChecker checks the folder a backup of a remote reads where
// both the repository and the remote are known: RunJob and the schedule
// input ask it (implemented by *restic.Service).
type RemoteSourceChecker interface {
	// CheckRemoteSource checks the folder path on remoteID against owner's
	// repository repoID: domain.ErrNotFound (restic.ErrUnknownRepository,
	// restic.ErrUnknownSourceRemote) for objects that are not owner's, a
	// *domain.ValidationError on "remote_id" (a Local directory remote) or
	// "path" (the source is, holds or is inside the repository's folder).
	CheckRemoteSource(ctx context.Context, owner domain.UserID, repoID domain.ResticRepoID, remoteID domain.RemoteID, path string) error
}

var _ ResticService = (*restic.Service)(nil)

// RepositoryNames names repositories for display: a job's or a schedule's
// repository (Job.repository, Schedule.repository).
type RepositoryNames interface {
	// Names returns display references of any repositories, of any owner,
	// deleted ones included. Callers show them only on objects the caller
	// may already see.
	Names(ctx context.Context, ids []domain.ResticRepoID) (map[domain.ResticRepoID]restic.Ref, error)
}
