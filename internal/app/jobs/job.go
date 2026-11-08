package jobs

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Job is one run of a job definition on a worker.
type Job struct {
	ID       domain.JobID
	WorkerID domain.WorkerID
	// OwnerID is the user who ran the job.
	OwnerID domain.UserID
	// Kind is the JobDefinition (and JobSpec) operation name, e.g.
	// "rclone_transfer".
	Kind   string
	Status domain.JobStatus
	// Definition is the encoded job definition (protojson of
	// syncr.v1.JobDefinition, see internal/jobkind). It contains no
	// secrets; the dispatcher builds the worker's spec from it.
	Definition []byte
	// RemoteIDs are the remotes the definition refers to directly (not the
	// members of unions among them), without duplicates, sorted by ID. They
	// were live remotes of the owner when the job was created and may have
	// been deleted since.
	RemoteIDs []domain.RemoteID
	// SearchText is what ListFilter.Query matches: the kind, the
	// definition's search terms (mode, paths) and the remotes' names at
	// creation time, lower-cased, one per line.
	SearchText string
	// ScheduleID is the schedule that launched the job; zero for a job run
	// by hand. It never changes, except that deleting the schedule clears
	// it (the job's history stays).
	ScheduleID domain.ScheduleID

	// Epoch is the worker epoch that accepted the job; empty while queued.
	// Dispatched and running jobs always have one.
	Epoch string
	// CancelRequestedAt is set when a cancel was requested for a dispatched
	// or running job (a queued job is cancelled at once instead). A job with
	// a cancel request is never queued again.
	CancelRequestedAt time.Time

	// Controller times, except StartedAt and FinishedAt, which come from the
	// worker's clock when it reported them (they are when things happened,
	// even if the controller learned later).
	CreatedAt    time.Time
	QueuedAt     time.Time // when the job (re-)entered the queue
	DispatchedAt time.Time // when the worker accepted it; zero while queued
	StartedAt    time.Time
	FinishedAt   time.Time

	// Requeues counts returns to the queue after a worker restart.
	Requeues int

	// Outcome, set when the job reaches a terminal status.
	Stats    Stats
	Error    JobError
	Warnings []string
	// ExitCode of the tool process; -1 if unknown or no process ran.
	ExitCode int
	// Output is the kind-specific output as an encoded link JobOutput; nil
	// if none.
	Output []byte

	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateJob. A created job has version 1.
	Version int64
}

// CancelRequested reports whether a cancel was requested.
func (j Job) CancelRequested() bool { return !j.CancelRequestedAt.IsZero() }

// Stats are cumulative job counters (see link.proto JobStats).
type Stats struct {
	Bytes      int64
	TotalBytes int64
	Files      int64
	TotalFiles int64
	Checks     int64
	Deletes    int64
	Renames    int64
	Errors     int64
	Elapsed    time.Duration
}

// Sample is a progress sample of a running job.
type Sample struct {
	JobID domain.JobID
	// Time is when the worker took the sample (worker clock).
	Time  time.Time
	Stats Stats
}

// JobError says why a job failed, was interrupted or cancelled. Code is the
// lower-case name of a link JobErrorCode without its prefix (e.g.
// "invalid_spec", "worker_restarted"); unknown or unset codes are
// "unspecified".
type JobError struct {
	Code    string
	Message string
}

// IsZero reports whether e is unset.
func (e JobError) IsZero() bool { return e == JobError{} }

// Error codes the controller sets itself. They match link JobErrorCode
// names.
const (
	CodeUnspecified     = "unspecified"
	CodeInternal        = "internal"
	CodeCancelled       = "cancelled"
	CodeWorkerShutdown  = "worker_shutdown"
	CodeDispatchTimeout = "dispatch_timeout"
	CodeWorkerRestarted = "worker_restarted"
	CodeWorkerLost      = "worker_lost"
	CodeWorkerRemoved   = "worker_removed"
	// CodeInvalidSpec: the job's definition cannot be decoded or built.
	CodeInvalidSpec = "invalid_spec"
	// CodeRemoteUnavailable: a remote of the job was deleted or is no
	// longer usable by its owner when the job was dispatched.
	CodeRemoteUnavailable = "remote_unavailable"
)

// Outcome is a worker-reported terminal result (Finish).
type Outcome struct {
	// Status is the terminal status to record.
	Status   domain.JobStatus
	Error    JobError
	Stats    Stats
	Warnings []string
	ExitCode int
	// StartedAt and FinishedAt are the worker's times; zero if unknown.
	StartedAt  time.Time
	FinishedAt time.Time
	Output     []byte
}

// Update is a change of a job published to watchers. Exactly one of Job and
// Progress is set.
type Update struct {
	JobID domain.JobID
	// Job is the job after a stored change (status, cancel request).
	Job *Job
	// Progress is a live sample; it may not be stored.
	Progress *Sample
}

// CreateParams are the inputs of Create. The owner is the actor.
type CreateParams struct {
	// ID is a preset job ID; zero mints a new one. Only the scheduler sets
	// it: it reserves the ID on its run before creating the job, so a
	// retried launch creates the job once (Create then fails with
	// ErrAlreadyExists, which the scheduler counts as success). The API
	// never sets it.
	ID domain.JobID
	// ScheduleID is the schedule launching the job; zero for a job run by
	// hand. It must exist (ErrNotFound otherwise).
	ScheduleID domain.ScheduleID
	WorkerID   domain.WorkerID
	// Kind is the definition's operation name (the caller derives it from
	// the definition).
	Kind string
	// Definition is the encoded job definition (see Job.Definition), checked
	// by the caller (internal/jobkind).
	Definition []byte
	// RemoteIDs are the remotes the definition refers to (jobkind.Refs);
	// each must be a live remote of the actor.
	RemoteIDs []domain.RemoteID
	// SearchTerms are the definition's texts a search matches
	// (jobkind.Refs); the service adds the kind and the remote names.
	SearchTerms []string
}

// ListFilter selects jobs for List. Zero fields do not filter.
type ListFilter struct {
	// OwnerID keeps only this user's jobs. List sets it to the actor unless
	// the actor is an admin and AllUsers is set (then zero means every
	// user).
	OwnerID domain.UserID
	// AllUsers asks for every user's jobs; only admins may (List ignores it
	// for other actors). Repositories ignore it.
	AllUsers bool
	// WorkerID keeps only this worker's jobs.
	WorkerID domain.WorkerID
	// Statuses keeps only jobs in one of these.
	Statuses []domain.JobStatus
	// RemoteID keeps only jobs whose RemoteIDs contain it.
	RemoteID domain.RemoteID
	// ScheduleID keeps only jobs launched by this schedule.
	ScheduleID domain.ScheduleID
	// Kinds keeps only jobs of one of these kinds.
	Kinds []string
	// Query keeps only jobs whose SearchText contains it as a plain
	// substring (no wildcards: repositories escape LIKE patterns). List
	// trims and lower-cases it, so matching is case-insensitive.
	Query string
	// CreatedAfter keeps only jobs created at or after it; CreatedBefore
	// only jobs created before it.
	CreatedAfter  time.Time
	CreatedBefore time.Time
	// Before is a keyset cursor: only jobs with ID < Before. Zero starts at
	// the newest job.
	Before domain.JobID
	// Limit is the page size: 0 means DefaultPageSize, at most MaxPageSize.
	// The repository receives the exact number of rows to return.
	Limit int
}

// Limits of List.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
	// MaxQueryLen bounds ListFilter.Query, in bytes.
	MaxQueryLen = 256
	// MaxKinds bounds ListFilter.Kinds.
	MaxKinds = 32
)

// Page is one page of List.
type Page struct {
	Jobs []Job
	// Next is the cursor (ListFilter.Before) of the next page; zero on the
	// last page.
	Next domain.JobID
}

// BucketUnit is the size of an activity bucket. The string values are the
// Postgres date_trunc units.
type BucketUnit string

// Bucket units.
const (
	// BucketHour buckets by hour of the activity's time zone.
	BucketHour BucketUnit = "hour"
	// BucketDay buckets by local day (23 to 25 hours across DST changes).
	BucketDay BucketUnit = "day"
)

// Valid reports whether u is a known unit.
func (u BucketUnit) Valid() bool { return u == BucketHour || u == BucketDay }

// ActivityFilter selects the finished jobs Activity counts.
type ActivityFilter struct {
	// From and Until bound the jobs' FinishedAt: [From, Until).
	From, Until time.Time
	// Unit is the bucket size.
	Unit BucketUnit
	// Location is the time zone whose hours or days the buckets follow
	// (an IANA zone, never time.Local).
	Location *time.Location
	// AllUsers counts every user's jobs; only admins may (Activity ignores
	// it for other actors, as List does).
	AllUsers bool
}

// ActivityBucket counts the jobs that finished in one bucket, by terminal
// status, with the bytes and files they transferred.
type ActivityBucket struct {
	// Start is the first instant of the bucket (an hour or a local day in
	// the filter's Location).
	Start                                     time.Time
	Succeeded, Failed, Interrupted, Cancelled int64
	Bytes, Files                              int64
}
