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
	// CreateJob inserts j with its RemoteIDs, in one transaction (Version
	// is stored as given, normally 1). ErrAlreadyExists if j.ID is taken;
	// ErrNotFound if j.WorkerID does not exist or is removed (checked in the
	// same statement, e.g. INSERT ... SELECT ... WHERE EXISTS (worker not
	// removed)), or if the owner, one of the remotes or a non-zero
	// ScheduleID does not exist (foreign keys; a deleted remote still
	// exists).
	CreateJob(ctx context.Context, j Job) error

	// GetJob returns a job. ErrNotFound if missing. Every method returning
	// jobs returns them with their RemoteIDs (sorted by ID) and SearchText.
	GetJob(ctx context.Context, id domain.JobID) (Job, error)

	// UpdateJob is an optimistic, version-checked update: if the stored
	// job's Version equals j.Version, it writes every mutable field of j
	// (Status, Epoch, CancelRequestedAt, QueuedAt, DispatchedAt, StartedAt,
	// FinishedAt, Requeues, Stats, Error, Warnings, ExitCode, Output), sets
	// Version = j.Version + 1 and returns the stored job. The immutable
	// fields (ID, WorkerID, OwnerID, Kind, Definition, CreatedAt,
	// ScheduleID) are never written, nor are RemoteIDs and SearchText.
	// ErrConflict if the version differs (the caller re-reads and
	// re-decides), ErrNotFound if missing. SQL: UPDATE jobs SET ...,
	// version = version + 1 WHERE id = $1 AND version = $2 RETURNING *.
	UpdateJob(ctx context.Context, j Job) (Job, error)

	// ListJobs returns at most f.Limit (negative: no limit) jobs matching
	// every set field of f, by ID descending (IDs are UUIDv7, so newest
	// first). f.Before and f.CreatedBefore are exclusive, f.CreatedAfter is
	// inclusive. f.Query is a literal substring of SearchText: "%", "_" and
	// "\" in it match themselves. f.AllUsers is ignored (the service has
	// resolved it into f.OwnerID).
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

	// LatestJobSamples returns the LatestJobSample of each of the jobs in
	// one query, keyed by job ID; jobs without samples (or unknown ones)
	// are absent. Repeated IDs are allowed.
	LatestJobSamples(ctx context.Context, ids []domain.JobID) (map[domain.JobID]Sample, error)

	// ListJobSamples returns all samples of the job by Time ascending.
	ListJobSamples(ctx context.Context, id domain.JobID) ([]Sample, error)
}

// ActivityRepository aggregates finished jobs for the dashboard. Same rules
// as Repository.
type ActivityRepository interface {
	// JobActivity counts the jobs whose FinishedAt is in [q.From, q.Until)
	// (of q.OwnerID; zero means every owner) by bucket and status, with the
	// sums of their Stats.Bytes and Stats.Files, ordered by bucket, then
	// status. Bucket is the start of the job's hour or local day in
	// q.Location, exactly as Postgres computes date_trunc(q.Unit,
	// finished_at, q.Location.String()): an ambiguous local time (DST
	// fall-back) resolves to the later instant, a non-existent one (spring
	// forward) with the offset before the transition. Buckets without jobs
	// are omitted.
	JobActivity(ctx context.Context, q ActivityQuery) ([]ActivityRow, error)

	// CountUnfinished returns the number of the owner's (zero: every
	// owner's) queued, dispatched and running jobs, by status; statuses
	// without jobs are omitted.
	CountUnfinished(ctx context.Context, owner domain.UserID) (map[domain.JobStatus]int64, error)
}

// ActivityQuery selects the jobs of ActivityRepository.JobActivity.
type ActivityQuery struct {
	From, Until time.Time
	Unit        BucketUnit
	// Location is an IANA zone (its String is passed to Postgres).
	Location *time.Location
	// OwnerID keeps only this user's jobs; zero means every owner.
	OwnerID domain.UserID
}

// ActivityRow is one (bucket, status) group of JobActivity.
type ActivityRow struct {
	// Bucket is the first instant of the bucket.
	Bucket time.Time
	Status domain.JobStatus
	// Jobs counts the group's jobs; Bytes and Files sum their stats.
	Jobs, Bytes, Files int64
}

// RemoteChecker checks the remotes a new job uses (*remotes.Service).
type RemoteChecker interface {
	// Usable returns the names of ids, which must all be live remotes of
	// owner: ErrNotFound if any is missing, deleted or another user's.
	Usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error)
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

// Notifier is told after a job reached a terminal status (any). It must not
// block: it only wakes background loops (the notification planner, the
// scheduler). Delivery is durable elsewhere: the planner finds finished
// jobs that were not notified yet in the store, so a lost call only delays
// a notification until the planner's next tick.
type Notifier interface {
	// JobFinished is called once per job, after the change to a terminal
	// status was stored, on the caller's goroutine (often the link ingest).
	JobFinished(j Job)
}

// NopNotifier is a Notifier that does nothing.
type NopNotifier struct{}

// JobFinished does nothing.
func (NopNotifier) JobFinished(Job) {}

// Waker wakes the command long polls of a worker after its pending commands
// changed (a job was queued or re-queued, a cancel was requested). The
// dispatcher implements it; it must not block.
type Waker interface {
	Wake(workerID domain.WorkerID)
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
