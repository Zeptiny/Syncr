package jobs

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists jobs. Implementations must be safe for concurrent use.
// Every method is atomic on its own; no use case needs a transaction spanning
// several calls. Errors wrap the domain sentinels named below; anything else
// is an internal error. Returned values must not alias storage.
type Repository interface {
	// CreateJob inserts j (Version is stored as given, normally 1).
	// ErrAlreadyExists if j.ID is taken; ErrNotFound if j.WorkerID does not
	// exist or is removed (checked in the same statement, e.g. INSERT ...
	// SELECT ... WHERE EXISTS (worker not removed)).
	CreateJob(ctx context.Context, j Job) error

	// GetJob returns a job. ErrNotFound if missing.
	GetJob(ctx context.Context, id domain.JobID) (Job, error)

	// UpdateJob is an optimistic, version-checked update: if the stored
	// job's Version equals j.Version, it writes every mutable field of j
	// (Status, Epoch, CancelRequestedAt, QueuedAt, DispatchedAt, StartedAt,
	// FinishedAt, Requeues, Stats, Error, Warnings, ExitCode, Output), sets
	// Version = j.Version + 1 and returns the stored job. The immutable
	// fields (ID, WorkerID, Kind, SpecEnc, CreatedAt) are never written.
	// ErrConflict if the version differs (the caller re-reads and
	// re-decides), ErrNotFound if missing. SQL: UPDATE jobs SET ...,
	// version = version + 1 WHERE id = $1 AND version = $2 RETURNING *.
	UpdateJob(ctx context.Context, j Job) (Job, error)

	// ListJobs returns at most f.Limit jobs matching f, by ID descending
	// (IDs are UUIDv7, so newest first). f.Before is exclusive.
	ListJobs(ctx context.Context, f ListFilter) ([]Job, error)

	// ListUnfinishedJobs returns the worker's queued, dispatched and running
	// jobs by ID ascending (oldest first). Hot path of the dispatcher: index
	// (worker_id) WHERE status IN (...).
	ListUnfinishedJobs(ctx context.Context, workerID domain.WorkerID) ([]Job, error)

	// ListQueuedJobs returns at most limit queued jobs (any worker) with
	// QueuedAt < before, by QueuedAt then ID ascending.
	ListQueuedJobs(ctx context.Context, before time.Time, limit int) ([]Job, error)
}

// SampleRepository persists progress samples. Same rules as Repository.
type SampleRepository interface {
	// AppendJobSample stores s. It is idempotent on (JobID, Time): storing
	// a sample whose key exists is a no-op without error (SQL: INSERT ...
	// ON CONFLICT DO NOTHING). ErrNotFound if the job does not exist.
	AppendJobSample(ctx context.Context, s Sample) error

	// LatestJobSample returns the job's sample with the greatest Time;
	// ok is false if it has none.
	LatestJobSample(ctx context.Context, id domain.JobID) (s Sample, ok bool, err error)

	// ListJobSamples returns all samples of the job by Time ascending.
	ListJobSamples(ctx context.Context, id domain.JobID) ([]Sample, error)
}

// Broker fans job updates out to live watchers within the process.
// Implementations must be safe for concurrent use.
type Broker interface {
	// Publish delivers u to the current subscribers of u.JobID. It never
	// blocks: a slow subscriber may miss intermediate Progress updates but
	// never a Job update. Concurrent changes of one job may publish their
	// Job updates out of order; consumers order them by Job.Version (Watch
	// does).
	Publish(u Update)
	// Subscribe starts receiving updates of job id. The caller must Close
	// the subscription.
	Subscribe(id domain.JobID) Subscription
}

// Subscription is a stream of updates of one job.
type Subscription interface {
	// Next blocks until an update is available, then returns the oldest
	// one. It returns ctx's error when ctx ends first, or an error after
	// Close.
	Next(ctx context.Context) (Update, error)
	// Close stops the subscription; it is idempotent.
	Close()
}

// Notifier reports noteworthy job outcomes to people (Milestone 4 adds
// channels; NopNotifier until then).
type Notifier interface {
	// JobFailed is called once after a job ended failed or interrupted. It
	// runs on the caller's goroutine (often the link ingest), so it must
	// return quickly; an error is only logged.
	JobFailed(ctx context.Context, j Job) error
}

// NopNotifier is a Notifier that does nothing.
type NopNotifier struct{}

// JobFailed does nothing.
func (NopNotifier) JobFailed(context.Context, Job) error { return nil }

// Waker wakes the command long polls of a worker after its pending commands
// changed (a job was queued or re-queued, a cancel was requested). The
// dispatcher implements it; it must not block.
type Waker interface {
	Wake(workerID domain.WorkerID)
}

// Cipher encrypts secrets at rest; associated data binds a ciphertext to its
// record. internal/adapters/secrets implements it.
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints job IDs. They must be time-ordered (UUIDv7) for List's
// newest-first order.
type IDGenerator interface {
	NewJobID() domain.JobID
}
