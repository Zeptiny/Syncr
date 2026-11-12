package restic

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists repositories and their snapshot cache.
// Implementations must be safe for concurrent use; every method is atomic
// on its own. Errors wrap the domain sentinels named below; anything else
// is an internal error. Returned values must not alias storage.
type Repository interface {
	// CreateRepo inserts r as a live repository that is not deleting: ID,
	// OwnerID, Name, Description, RemoteID, Path, PasswordEnc (required),
	// ResticID, FormatVersion, CreatedAt, UpdatedAt and Version (stored as
	// given, normally 1); the deletion fields, DeletedAt and Stats are not
	// written. ErrInvalidArgument for values the schema rejects (name or
	// description length, a ResticID that is not 64 lower-case hex digits,
	// no PasswordEnc). ErrAlreadyExists if r.ID is taken or, among live
	// repositories, the
	// owner's name (case-insensitive), the location (RemoteID, Path) or
	// the owner's ResticID is taken; ErrNotFound if the owner does not
	// exist, or the remote does not exist or is deleted (checked in the
	// same statement: INSERT ... SELECT ... WHERE EXISTS).
	CreateRepo(ctx context.Context, r Repo) error

	// GetRepo returns a repository, deleted ones included, with its
	// Stats. ErrNotFound if it never existed.
	GetRepo(ctx context.Context, id domain.ResticRepoID) (Repo, error)

	// GetRepoRow is GetRepo without the Stats (zero): the cheap read of
	// ownership checks and of Resolve.
	GetRepoRow(ctx context.Context, id domain.ResticRepoID) (Repo, error)

	// GetRepos returns the repositories of ids that exist (deleted ones
	// included), by ID; missing ones are omitted and duplicates returned
	// once. Their Stats are not needed (zero is fine).
	GetRepos(ctx context.Context, ids []domain.ResticRepoID) ([]Repo, error)

	// ListRepos returns the live repositories matching f, deleting ones
	// included, by lower-cased name, then ID, with their Stats.
	ListRepos(ctx context.Context, f ListFilter) ([]Repo, error)

	// ListRepoRows is ListRepos without the Stats (zero): for checks of
	// names and locations.
	ListRepoRows(ctx context.Context, f ListFilter) ([]Repo, error)

	// UpdateRepo writes Name, Description, PasswordEnc and UpdatedAt of a
	// live repository if its stored Version equals r.Version, sets Version
	// = r.Version + 1 and returns the stored repository (with Stats).
	// ErrConflict if the version differs; ErrNotFound if missing or
	// deleted; ErrAlreadyExists if the new name is taken.
	UpdateRepo(ctx context.Context, r Repo) (Repo, error)

	// DeleteRepo soft-deletes a live repository that is not deleting
	// ("forget only"): DeletedAt = at, UpdatedAt = at, PasswordEnc = nil,
	// Version + 1, and it removes the cached snapshots, in one
	// transaction. ErrNotFound if missing or deleted;
	// ErrFailedPrecondition while Deleting.
	DeleteRepo(ctx context.Context, id domain.ResticRepoID, at time.Time) error

	// MarkDeleting moves a live repository that is not deleting to
	// Deleting, with DeletionJobID = jobID (not zero: ErrInvalidArgument),
	// DeletionRequestedAt = at,
	// DeletionError = "", DeletionCancelled = false, UpdatedAt = at and
	// Version + 1. In the same
	// transaction it pauses the repository's enabled schedules (Enabled
	// false, NextRunAt zero, PausedReason "repository_deleting", UpdatedAt
	// at, Version + 1) and cancels their open runs (status cancelled,
	// reason "repository_deleting", UpdatedAt at). It returns the stored
	// repository. ErrNotFound if missing or deleted; ErrConflict if it is
	// already deleting (the caller re-reads it).
	MarkDeleting(ctx context.Context, id domain.ResticRepoID, jobID domain.JobID, at time.Time) (Repo, error)

	// ListPendingDeletions returns, oldest DeletionRequestedAt first (then
	// by ID) and at most limit (negative: no limit), the deleting
	// repositories whose deletion job is terminal (succeeded, failed,
	// cancelled, interrupted), or whose job does not exist and was
	// requested before startedBefore.
	ListPendingDeletions(ctx context.Context, startedBefore time.Time, limit int) ([]PendingDeletion, error)

	// FinishDeletion ends a deletion. It is a compare-and-swap on Deleting
	// && DeletionJobID == jobID (ErrConflict otherwise, also for a missing
	// repository):
	//   - o.Succeeded: one transaction sets DeletedAt = at, DataDeletedAt
	//     = at, UpdatedAt = at, PasswordEnc = nil, Deleting = false,
	//     Version + 1, and removes the cached snapshots;
	//   - otherwise: Deleting = false, DeletionError = o.Error (clipped to
	//     MaxDeletionErrorLen runes), DeletionCancelled = o.Cancelled,
	//     UpdatedAt = at, Version + 1. The
	//     password, DeletionJobID and the paused schedules are kept.
	FinishDeletion(ctx context.Context, id domain.ResticRepoID, jobID domain.JobID, o DeletionOutcome, at time.Time) error

	// ReplaceSnapshots atomically replaces the snapshot cache of a live
	// repository (deleting ones included) with snaps and records
	// refreshedAt, truncated and mark (the repository's ChangeMark when the
	// listing started). ErrNotFound if missing or deleted;
	// ErrInvalidArgument for a snapshot ID that is not 64 lower-case hex
	// digits, ErrAlreadyExists for one listed twice (nothing changes then).
	// Concurrent replaces leave one of the sets, never a mix.
	ReplaceSnapshots(ctx context.Context, id domain.ResticRepoID, snaps []Snapshot, truncated bool, refreshedAt time.Time, mark ChangeMark) error

	// ListSnapshots returns the cache, newest first (by Time descending,
	// then ID), with RefreshedAt, Truncated and Mark as stored, and the
	// repository's CurrentMark (Stale is left to the service). It works for
	// deleted repositories too (their cache is empty). ErrNotFound if the
	// repository never existed.
	ListSnapshots(ctx context.Context, id domain.ResticRepoID) (SnapshotList, error)
}

// RemoteChecker checks the remote of a new repository (*remotes.Service).
type RemoteChecker interface {
	// RemoteType returns the backend type ID of a live remote of owner;
	// ErrNotFound otherwise (missing, deleted or another user's).
	RemoteType(ctx context.Context, owner domain.UserID, id domain.RemoteID) (string, error)
}

// Target is where a probe finds a repository. It holds the password: never
// log it.
type Target struct {
	// RepositoryID is zero for a repository that is not stored yet
	// (Create).
	RepositoryID domain.ResticRepoID
	RemoteID     domain.RemoteID
	Path         string
	Password     string
	// WorkerID is the worker to run on; zero lets the prober pick.
	WorkerID domain.WorkerID
}

// VerifyResult is what Prober.Verify found: the repository's config and the
// worker that read it.
type VerifyResult struct {
	ResticID      string
	FormatVersion int
	WorkerID      domain.WorkerID
	WorkerName    string
}

// SnapshotsResult is what Prober.Snapshots listed.
type SnapshotsResult struct {
	// Snapshots are in any order (the service sorts them).
	Snapshots []Snapshot
	// Truncated: only the newest MaxSnapshots were listed.
	Truncated  bool
	WorkerID   domain.WorkerID
	WorkerName string
}

// Prober runs restic on a worker for these use cases (*probe.Runner in the
// controller, which resolves the remote for owner and picks the worker).
//
// Errors: *ProbeError for what restic or the worker reported;
// domain.ErrNotFound (the remote or the worker), domain.ErrUnavailable (the
// worker is offline, busy or did not answer in time),
// domain.ErrFailedPrecondition (no usable worker, an outdated worker),
// domain.ErrRateLimited.
type Prober interface {
	// Verify runs restic init (init true) or cat config on t and returns
	// the repository's config.
	Verify(ctx context.Context, owner domain.UserID, t Target, init bool) (VerifyResult, error)
	// Snapshots lists t's snapshots (restic --no-lock snapshots).
	Snapshots(ctx context.Context, owner domain.UserID, t Target) (SnapshotsResult, error)
	// List lists the directory path (a tree path) of snapshot snapshotID
	// (restic --no-lock ls).
	List(ctx context.Context, owner domain.UserID, t Target, snapshotID, path string) (Listing, error)
}

// ProbeError is a failure reported by the worker. Code is a link error code
// name ("wrong_password", "repository_not_found", "already_exists",
// "path_not_allowed", "not_found", "repository_locked", "timeout",
// "execution_failed", ...); Message is scrubbed of secrets.
type ProbeError struct {
	Code    string
	Message string
}

// Error implements error: "<code>: <message>".
func (e *ProbeError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Cipher encrypts passwords at rest; associated data binds a ciphertext to
// its record. internal/adapters/secrets implements it.
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints repository IDs and the reserved IDs of deletion jobs
// (UUIDv7).
type IDGenerator interface {
	NewResticRepoID() domain.ResticRepoID
	NewJobID() domain.JobID
}

// Deletions starts a repository's restic_delete_data job (*probe.DataDeleter
// in the controller). It takes plain values, because app packages cannot
// use the job kinds or the jobs service.
type Deletions interface {
	// Pick chooses the worker without changing anything: workerID if set,
	// otherwise the job pick rules (an online worker with the kind and its
	// features, preferring the one of the repository's newest succeeded
	// job). A remote on a worker's local filesystem needs workerID.
	// Errors: ErrNotFound (worker or remote), ErrUnavailable (offline),
	// ErrFailedPrecondition (no usable worker, or an outdated one).
	Pick(ctx context.Context, owner domain.UserID, remoteID domain.RemoteID, workerID domain.WorkerID) (domain.WorkerID, error)
	// Start queues the job with the preset jobID on workerID. It is
	// idempotent by jobID: a job that already exists counts as success.
	// The job counts a location without restic's files as already deleted
	// only when an earlier deletion job of the repository ran on the same
	// worker and deleted files there; otherwise (a first attempt, or
	// earlier ones that deleted nothing or ran elsewhere) the job then
	// fails, deleting nothing, and the repository becomes active again.
	// Errors: as Pick, plus transient store errors.
	Start(ctx context.Context, owner domain.UserID, repoID domain.ResticRepoID, jobID domain.JobID, workerID domain.WorkerID) error
}
