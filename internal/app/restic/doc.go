// Package restic owns restic repositories (Milestone 5): a path on one of a
// user's remotes that holds a restic repository, with its password; the
// cached snapshot list of each repository; browsing a snapshot's tree; and
// the opt-in deletion of a repository's data in the backend.
//
// # Rules
//
//   - Repositories are strictly private to their owner, admins included:
//     another user's repository is domain.ErrNotFound for every use case.
//   - The password is write-only. It is encrypted with the Cipher
//     (associated data PasswordAD), never returned, logged, put in a job
//     definition or stored on a worker; it exists only in Resolved and
//     Target values and in the job or probe spec built from them.
//   - Among live repositories these are unique: the name per owner
//     (case-insensitive), the location (RemoteID, Path), and the restic ID
//     (the repository's config ID) per owner, so one repository cannot be
//     imported twice under two remotes.
//   - The path is relative to the remote's root ("" is the root) and
//     normalized by NormalizePath. Location and remote never change: a new
//     location is a new repository.
//   - The remote must be a live remote of the owner whose backend type can
//     hold repositories (backend capability ResticRepository). A
//     repository on a local remote is tied to the worker whose disk holds
//     it: creating it needs a WorkerID.
//
// # Service (m5: Track C implements it in service.go and delete.go)
//
// The Service is constructed as New(Deps{Repo Repository; Remotes
// RemoteChecker; Prober Prober; Backends *backend.Registry; Deletions
// Deletions; Cipher Cipher; Clock Clock; IDs IDGenerator; Logger
// *slog.Logger}, Config). Deletions may be nil: DeleteData then fails with
// ErrFailedPrecondition ("data deletion is not available"). Its use cases:
//
//	// Create validates in, pre-checks the name and location against the
//	// owner's live repositories (so a taken name is never initialized
//	// first), runs Prober.Verify (init or cat config) on a worker, maps a
//	// *ProbeError to field errors ("password", "path", "mode",
//	// "worker_id"; anything else ErrFailedPrecondition), then seals the
//	// password and stores the repository with the probe's ResticID and
//	// FormatVersion.
//	Create(ctx, actor domain.Actor, in Input) (Repo, error)
//	Get(ctx, actor domain.Actor, id domain.ResticRepoID) (Repo, error)
//	List(ctx, actor domain.Actor, f ListFilter) ([]Repo, error)
//	// Update renames it; a new password is verified (cat config) first and
//	// must open the same repository (ResticID).
//	Update(ctx, actor domain.Actor, id domain.ResticRepoID, version int64, u Update) (Repo, error)
//	// Delete forgets the repository (a soft delete: DeleteRepo), or with
//	// o.DeleteData starts a restic_delete_data job (MarkDeleting, then
//	// Deletions.Start); see "Data deletion" below.
//	Delete(ctx, actor domain.Actor, id domain.ResticRepoID, o DeleteOptions) (DeleteResult, error)
//	// Run completes data deletions every 30s and on Wake (non-blocking);
//	// FinishDeletions runs one pass.
//	Run(ctx) error
//	Wake()
//	FinishDeletions(ctx) (int, error)
//	// Snapshots serves the cached list, refreshing it through a worker
//	// (Prober.Snapshots, then ReplaceSnapshots) when o.Refresh is set or it
//	// was never listed; a failed refresh returns the cache with
//	// RefreshError when it has rows.
//	Snapshots(ctx, actor domain.Actor, id domain.ResticRepoID, o SnapshotsOptions) (SnapshotList, error)
//	// Browse lists one directory of a snapshot (Prober.List).
//	Browse(ctx, actor domain.Actor, id domain.ResticRepoID, snapshotID, path string, workerID domain.WorkerID) (Listing, error)
//
// and, for other use cases, with plain values (app packages cannot import
// each other):
//
//	// UsableRepository is the jobs and schedules RepositoryChecker: the
//	// repository must be a live repository of owner (ErrNotFound
//	// otherwise). A deleting repository is usable only by its own deletion
//	// job (jobID == DeletionJobID); otherwise ErrRepositoryDeleting.
//	UsableRepository(ctx, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (name string, remoteID domain.RemoteID, err error)
//	// Names returns display references of any owner's repositories,
//	// deleted ones included.
//	Names(ctx, ids []domain.ResticRepoID) (map[domain.ResticRepoID]Ref, error)
//	// Resolve returns a live repository of owner with its decrypted
//	// password (ErrNotFound otherwise; ErrFailedPrecondition if the
//	// password does not decrypt), for job and probe specs.
//	Resolve(ctx, owner domain.UserID, id domain.ResticRepoID) (Resolved, error)
//
// # Data deletion
//
// "Forget only" (the default) soft-deletes the row: DeletedAt is set, the
// password and the snapshot cache are dropped, the backend data is kept.
// With DeleteOptions.DeleteData (ConfirmName equal to the name) the
// repository moves to Deleting (MarkDeleting pauses its schedules and
// cancels their open runs in the same transaction) and a restic_delete_data
// job deletes restic's own files from the backend. The row is
// soft-deleted, and the password wiped, only when that job succeeds
// (FinishDeletion). A failed, cancelled or interrupted job, or one that
// was never created within DeletionGrace, reverts the repository to active
// with DeletionError set; its schedules stay paused.
//
//	active --Delete(DeleteData)--> deleting --job succeeded--> deleted
//	                                  |
//	                                  +--job failed / never created--> active (DeletionError)
//
// While deleting, only the deletion job may use the repository
// (UsableRepository); Update, a refreshing Snapshots and Browse fail with
// ErrRepositoryDeleting; a repeated DeleteData returns the same job.
//
// Allowed dependencies: the standard library, internal/domain and
// internal/backend (the core API); every other collaborator is a port
// declared in ports.go.
package restic
