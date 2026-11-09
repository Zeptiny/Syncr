package restic

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Mode is how Create finds a new repository.
type Mode string

// Create modes.
const (
	// ModeInit creates the repository (restic init); the location must not
	// hold one.
	ModeInit Mode = "init"
	// ModeConnect uses an existing repository (restic cat config with the
	// password): "import an existing repository".
	ModeConnect Mode = "connect"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeInit || m == ModeConnect }

// MutatingKinds returns the job kinds whose success makes the snapshot
// cache stale (SnapshotList.LastChangeAt): backup, forget and prune.
func MutatingKinds() []string {
	return []string{"restic_backup", "restic_forget", "restic_prune"}
}

// Job kinds the repository statistics name (RepoStats.LastBackup,
// LastCheck) and the deletion job's kind.
const (
	KindBackup     = "restic_backup"
	KindCheck      = "restic_check"
	KindDeleteData = "restic_delete_data"
)

// Repo is a stored restic repository.
type Repo struct {
	ID      domain.ResticRepoID
	OwnerID domain.UserID
	// Name is 1 to MaxNameLen characters, unique per owner among live
	// repositories (case-insensitive). Description is at most
	// MaxDescriptionLen characters.
	Name        string
	Description string
	// RemoteID and Path are the repository's location: a remote of the
	// owner and a path on it, normalized (NormalizePath), relative to the
	// remote's root. They never change.
	RemoteID domain.RemoteID
	Path     string
	// PasswordEnc is the password encrypted with the Cipher (associated
	// data PasswordAD(ID)); nil once the repository is deleted.
	PasswordEnc []byte
	// ResticID is restic's repository config ID (64 lower-case hex
	// digits); FormatVersion its format version (1 or 2; 0 unknown).
	ResticID      string
	FormatVersion int

	// Data deletion (see the package documentation). Deleting is true
	// while the restic_delete_data job DeletionJobID is pending. After a
	// failed attempt Deleting is false, and DeletionJobID,
	// DeletionRequestedAt and DeletionError describe that attempt.
	// DataDeletedAt is set when the data was deleted (the row is then
	// soft-deleted as well).
	Deleting            bool
	DeletionJobID       domain.JobID
	DeletionRequestedAt time.Time
	// DeletionError is scrubbed, at most MaxDeletionErrorLen runes.
	DeletionError string
	DataDeletedAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt is when the repository was deleted (forgotten or its data
	// deleted); zero while it is live.
	DeletedAt time.Time
	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateRepo. A created repository has
	// version 1.
	Version int64

	// Stats are computed by the store when it reads the repository
	// (GetRepo, ListRepos); they are never written.
	Stats RepoStats
}

// Deleted reports whether the repository has been deleted.
func (r Repo) Deleted() bool { return !r.DeletedAt.IsZero() }

// RepoStats are a repository's statistics from its snapshot cache, its jobs
// and its schedules.
type RepoStats struct {
	// SnapshotCount is the number of cached snapshots, LatestSnapshotAt the
	// newest one's time (zero if none).
	SnapshotCount    int
	LatestSnapshotAt time.Time
	// LatestSnapshotBytes is summary.total_bytes_processed of the newest
	// cached snapshot; 0 unknown.
	LatestSnapshotBytes int64
	// SnapshotsRefreshedAt is when the cache was last replaced (zero:
	// never); SnapshotsTruncated whether it holds only the newest
	// snapshots.
	SnapshotsRefreshedAt time.Time
	SnapshotsTruncated   bool
	// LastBackup and LastCheck are the newest job of kind restic_backup
	// and restic_check on the repository (any status, by job ID); zero if
	// none.
	LastBackup JobRef
	LastCheck  JobRef
	// ScheduleCount counts the schedules whose RepositoryID is the
	// repository.
	ScheduleCount int
}

// JobRef names a job for the repository statistics.
type JobRef struct {
	ID         domain.JobID
	Status     domain.JobStatus
	CreatedAt  time.Time
	FinishedAt time.Time
}

// Input describes a repository to create. It holds the password: never log
// it.
type Input struct {
	Name        string
	Description string
	RemoteID    domain.RemoteID
	// Path is relative to the remote's root; NormalizePath normalizes it.
	Path     string
	Password string
	Mode     Mode
	// WorkerID is the worker to initialize or verify on; optional, but
	// required for a remote on a worker's local filesystem.
	WorkerID domain.WorkerID
}

// Update changes a repository. Password "" keeps the stored one; a new one
// is verified on WorkerID (zero: the prober picks). It holds the password:
// never log it.
type Update struct {
	Name        string
	Description string
	Password    string
	WorkerID    domain.WorkerID
}

// Ref names a repository for display (a job's or a schedule's repository).
// It may be another user's or a deleted repository.
type Ref struct {
	ID       domain.ResticRepoID
	OwnerID  domain.UserID
	Name     string
	RemoteID domain.RemoteID
	Deleted  bool
}

// Resolved is a repository ready for a spec (dispatch, probes): its
// location and decrypted password. It exists only in memory; never log or
// store it.
type Resolved struct {
	ID       domain.ResticRepoID
	OwnerID  domain.UserID
	Name     string
	RemoteID domain.RemoteID
	Path     string
	Password string
	ResticID string
}

// Snapshot is one cached snapshot.
type Snapshot struct {
	// ID is the full snapshot ID (64 hex digits).
	ID   string
	Time time.Time
	// Hostname, Username, Parent (the parent snapshot's ID, "" if none)
	// and ProgramVersion are as restic recorded them.
	Hostname       string
	Username       string
	Parent         string
	ProgramVersion string
	// Paths are the backed-up paths in the backing-up host's form (a
	// Windows snapshot has "C:\Users\ada"); Tags its tags.
	Paths []string
	Tags  []string
	// Summary is nil if restic recorded none (before restic 0.17).
	Summary *SnapshotSummary
}

// SnapshotSummary is restic's summary of the backup that created a
// snapshot.
type SnapshotSummary struct {
	BackupStart, BackupEnd                   time.Time
	FilesNew, FilesChanged, FilesUnmodified  int64
	DirsNew, DirsChanged, DirsUnmodified     int64
	DataBlobs, TreeBlobs                     int64
	DataAdded, DataAddedPacked               int64
	TotalFilesProcessed, TotalBytesProcessed int64
}

// SnapshotList is a repository's snapshot cache.
type SnapshotList struct {
	// Snapshots are newest first (by Time, then ID).
	Snapshots []Snapshot
	// RefreshedAt is when the cache was last replaced; zero if never.
	RefreshedAt time.Time
	// Truncated: the worker listed only the newest MaxSnapshots.
	Truncated bool
	// LastChangeAt is the newest FinishedAt of a succeeded job of one of
	// MutatingKinds on the repository; zero if none.
	LastChangeAt time.Time
	// Stale is RefreshedAt.IsZero() || LastChangeAt.After(RefreshedAt),
	// computed by the service (stores leave it false).
	Stale bool
	// RefreshError is set when a refresh failed and the cache was
	// returned instead (scrubbed).
	RefreshError string
	// WorkerID and WorkerName name the worker that refreshed the cache in
	// this call; zero otherwise.
	WorkerID   domain.WorkerID
	WorkerName string
}

// SnapshotsOptions tune Service.Snapshots.
type SnapshotsOptions struct {
	// Refresh lists the snapshots through a worker first.
	Refresh bool
	// WorkerID is the worker to refresh through; zero lets the prober
	// pick.
	WorkerID domain.WorkerID
}

// EntryType is what a listed snapshot entry is. The string values are the
// API's names.
type EntryType string

// Entry types.
const (
	EntryFile    EntryType = "file"
	EntryDir     EntryType = "dir"
	EntrySymlink EntryType = "symlink"
	EntryOther   EntryType = "other"
)

// Entry is one entry of a listed snapshot directory.
type Entry struct {
	Name string
	// Path is the entry's snapshot tree path ("/srv/photos/a.jpg").
	Path    string
	Type    EntryType
	Size    int64
	ModTime time.Time
	// Mode is the permissions as "ls -l" shows them; may be empty.
	Mode string
}

// Listing is a listed snapshot directory.
type Listing struct {
	// WorkerID and WorkerName name the worker that listed it.
	WorkerID   domain.WorkerID
	WorkerName string
	// Path is the listed tree path.
	Path string
	// Entries are directories first, then by name.
	Entries []Entry
	// Truncated: only the first entries are listed.
	Truncated bool
}

// ListFilter selects repositories. Zero fields do not filter.
type ListFilter struct {
	// OwnerID keeps only this user's repositories (the service always sets
	// it to the actor).
	OwnerID domain.UserID
	// RemoteID keeps only repositories on this remote.
	RemoteID domain.RemoteID
}

// DeleteOptions choose how Delete deletes. When DeleteData is set,
// ConfirmName must equal the repository's Name exactly.
type DeleteOptions struct {
	// DeleteData also deletes restic's files in the backend (opt-in).
	DeleteData bool
	// ConfirmName is the typed confirmation of DeleteData.
	ConfirmName string
	// WorkerID is the worker to delete through; zero picks one (required
	// for a remote on a worker's local filesystem).
	WorkerID domain.WorkerID
}

// DeleteResult says how a deletion proceeds; it is zero for "forget only".
type DeleteResult struct {
	// DeletionJobID is the restic_delete_data job, WorkerID its worker.
	DeletionJobID domain.JobID
	WorkerID      domain.WorkerID
}

// PendingDeletion is a deleting repository whose deletion job has ended,
// or whose job was never created (Repository.ListPendingDeletions).
type PendingDeletion struct {
	RepoID      domain.ResticRepoID
	OwnerID     domain.UserID
	JobID       domain.JobID
	RequestedAt time.Time
	// JobStatus is the job's (terminal) status; "" when the job does not
	// exist.
	JobStatus domain.JobStatus
	// JobErrorCode and JobErrorMessage are the job's error (already
	// scrubbed by the worker).
	JobErrorCode    string
	JobErrorMessage string
}

// DeletionOutcome ends a deletion (Repository.FinishDeletion).
type DeletionOutcome struct {
	// Succeeded deletes the row; otherwise the repository becomes active
	// again with Error as its DeletionError.
	Succeeded bool
	Error     string
}

// Limits and policy constants.
const (
	// MaxNameLen bounds a repository name, in characters.
	MaxNameLen = 64
	// MaxDescriptionLen bounds a description, in characters.
	MaxDescriptionLen = 500
	// MaxPathLen bounds a normalized path, in bytes.
	MaxPathLen = 1024
	// MaxPasswordLen bounds a password, in bytes; MinInitPasswordLen is
	// the shortest password ModeInit accepts, in characters.
	MaxPasswordLen     = 4096
	MinInitPasswordLen = 8
	// MaxDeletionErrorLen bounds DeletionError, in runes.
	MaxDeletionErrorLen = 1000
	// MaxSnapshots is the most snapshots a refresh caches (the newest).
	MaxSnapshots = 5000
	// DeletionGrace is how long a deletion waits for its job to be created
	// before it is given up ("the deletion job was never created; try
	// again").
	DeletionGrace = 15 * time.Minute
	// FinishBatch is how many pending deletions one FinishDeletions pass
	// reads.
	FinishBatch = 100
)

// ErrUnknownRepository reports a repository that is not a live repository
// of the caller (missing, deleted or another user's). It matches
// domain.ErrNotFound.
var ErrUnknownRepository = fmt.Errorf("repository %w", domain.ErrNotFound)

// ErrRepositoryDeleting reports a repository whose data is being deleted:
// only its own restic_delete_data job (DeletionJobID) may still use it. It
// matches domain.ErrFailedPrecondition.
var ErrRepositoryDeleting = fmt.Errorf("the repository's data is being deleted: %w", domain.ErrFailedPrecondition)

// PasswordAD is the associated data binding a repository's encrypted
// password to its record ("restic-password:<id>").
func PasswordAD(id domain.ResticRepoID) []byte { return []byte("restic-password:" + id.String()) }

// NormalizePath normalizes a repository path, relative to its remote's
// root: surrounding white space, leading and trailing "/" and "."
// segments are removed and repeated "/" collapsed ("" is the root), so one
// location has one spelling (the location is unique). It rejects ".." segments,
// control characters, backslashes and results longer than MaxPathLen
// bytes, with a *domain.ValidationError on field "path".
func NormalizePath(p string) (string, error) {
	fail := func(msg string) (string, error) {
		var v domain.Validation
		v.Add("path", msg)
		return "", v.Err()
	}
	p = strings.TrimSpace(p)
	switch {
	case !utf8.ValidString(p):
		return fail("is not valid UTF-8")
	case strings.ContainsFunc(p, unicode.IsControl):
		return fail("must not contain control characters")
	case strings.Contains(p, `\`):
		return fail(`must use "/" to separate folders, not "\"`)
	}
	var segs []string
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return fail(`must not contain ".." segments`)
		}
		segs = append(segs, seg)
	}
	out := strings.Join(segs, "/")
	if len(out) > MaxPathLen {
		return fail(fmt.Sprintf("must be at most %d bytes", MaxPathLen))
	}
	return out, nil
}
