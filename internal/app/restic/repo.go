package restic

import (
	"fmt"
	"slices"
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

// MutatingKinds returns the job kinds that can change a repository's
// snapshots: backup, forget, prune and the deletion of its data. A job of
// these kinds changes the snapshot cache's ChangeMark when it ends after a
// worker accepted it, however it ended: a failed forget or prune may have
// removed snapshots, a backup may have saved its snapshot before it failed,
// and a failed or cancelled data deletion has usually removed snapshots/
// first (IsChange).
func MutatingKinds() []string {
	return []string{"restic_backup", "restic_forget", "restic_prune", KindDeleteData}
}

// IsChange reports whether a job counts in its repository's ChangeMark: it
// is of one of MutatingKinds, it ended (status terminal), and a worker had
// accepted it (dispatchedAt or startedAt set), so it may have run. A job
// that failed or was cancelled while queued never ran.
func IsChange(kind string, status domain.JobStatus, dispatchedAt, startedAt time.Time) bool {
	return status.Terminal() && (!dispatchedAt.IsZero() || !startedAt.IsZero()) && slices.Contains(MutatingKinds(), kind)
}

// ChangeMark tells whether a repository's snapshots may have changed since
// the snapshot cache was taken, without comparing clocks: the cache stores
// the repository's mark read before the worker was asked to list, and is
// stale once the repository's current mark differs.
//
// The mark counts the jobs on the repository that IsChange (jobs are never
// deleted and an ended job never changes, so the count only grows: every
// change that ends after the mark was read makes it differ), with the
// newest FinishedAt among them. The time is the worker's and is only
// compared for equality; it keeps a purge of old jobs together with as
// many new ones from looking like no change.
type ChangeMark struct {
	// Jobs is the number of such jobs.
	Jobs int64
	// LastFinishedAt is their newest FinishedAt; zero if there is none.
	LastFinishedAt time.Time
}

// Equal reports whether m and o are the same mark.
func (m ChangeMark) Equal(o ChangeMark) bool {
	return m.Jobs == o.Jobs && m.LastFinishedAt.Equal(o.LastFinishedAt)
}

// With returns m with one more change that finished at finishedAt.
func (m ChangeMark) With(finishedAt time.Time) ChangeMark {
	m.Jobs++
	if finishedAt.After(m.LastFinishedAt) {
		m.LastFinishedAt = finishedAt
	}
	return m
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
	// DeletionRequestedAt, DeletionError and DeletionCancelled describe
	// that attempt.
	// DataDeletedAt is set when the data was deleted (the row is then
	// soft-deleted as well).
	Deleting            bool
	DeletionJobID       domain.JobID
	DeletionRequestedAt time.Time
	// DeletionError is scrubbed, at most MaxDeletionErrorLen runes.
	DeletionError string
	// DeletionCancelled says the last attempt was cancelled, not failed;
	// it is only set together with DeletionError.
	DeletionCancelled bool
	DataDeletedAt     time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt is when the repository was deleted (forgotten or its data
	// deleted); zero while it is live.
	DeletedAt time.Time
	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateRepo. A created repository has
	// version 1.
	Version int64

	// Stats are computed by the store when it reads the repository with
	// them (GetRepo, ListRepos; zero from GetRepoRow, ListRepoRows); they
	// are never written.
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
	// SnapshotsMark is the ChangeMark stored with the cache, ChangeMark the
	// repository's current one (as SnapshotList.Mark and CurrentMark).
	SnapshotsMark ChangeMark
	ChangeMark    ChangeMark
	// LastBackup and LastCheck are the newest job of kind restic_backup
	// and restic_check on the repository (any status, by job ID); zero if
	// none.
	LastBackup JobRef
	LastCheck  JobRef
	// ScheduleCount counts the schedules whose RepositoryID is the
	// repository.
	ScheduleCount int
}

// SnapshotsStale reports whether the snapshot cache may be out of date: it
// was never filled, or a job that may have changed the snapshots ended
// since it was taken (its ChangeMark differs).
func (s RepoStats) SnapshotsStale() bool {
	return s.SnapshotsRefreshedAt.IsZero() || !s.SnapshotsMark.Equal(s.ChangeMark)
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
	// Mark is the repository's ChangeMark when the cache was taken (read
	// before the worker was asked); CurrentMark is the current one.
	Mark        ChangeMark
	CurrentMark ChangeMark
	// Stale is RefreshedAt.IsZero() || !Mark.Equal(CurrentMark), computed
	// by the service (stores leave it false).
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
	// again with Error as its DeletionError, and Cancelled as its
	// DeletionCancelled (the job was cancelled, not failed).
	Succeeded bool
	Error     string
	Cancelled bool
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
