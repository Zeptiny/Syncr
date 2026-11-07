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

// CreateParams are the inputs of Create.
type CreateParams struct {
	WorkerID domain.WorkerID
	// OwnerID is the user who runs the job.
	OwnerID domain.UserID
	// Kind is the definition's operation name (the caller derives it from
	// the definition).
	Kind string
	// Definition is the encoded job definition (see Job.Definition), checked
	// by the caller.
	Definition []byte
}

// ListFilter selects jobs for List.
type ListFilter struct {
	// WorkerID keeps only this worker's jobs; zero means all.
	WorkerID domain.WorkerID
	// Statuses keeps only jobs in one of these; empty means all.
	Statuses []domain.JobStatus
	// Before is a keyset cursor: only jobs with ID < Before. Zero starts at
	// the newest job.
	Before domain.JobID
	// Limit is the page size: 0 means DefaultPageSize, at most MaxPageSize.
	// The repository receives the exact number of rows to return.
	Limit int
}

// Page sizes for List.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Page is one page of List.
type Page struct {
	Jobs []Job
	// Next is the cursor (ListFilter.Before) of the next page; zero on the
	// last page.
	Next domain.JobID
}
