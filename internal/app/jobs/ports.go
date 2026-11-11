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
	// CreateJob inserts j with its RemoteIDs, RepositoryID and
	// ConcurrencyKeys (stored as given), in one transaction (Version is
	// stored as given, normally 1). ErrAlreadyExists if j.ID is taken;
	// ErrNotFound if j.WorkerID does not exist or is removed (checked in the
	// same statement, e.g. INSERT ... SELECT ... WHERE EXISTS (worker not
	// removed)), or if the owner, one of the remotes, a non-zero
	// ScheduleID or a non-zero RepositoryID does not exist (foreign keys;
	// deleted remotes and repositories still exist).
	CreateJob(ctx context.Context, j Job) error

	// GetJob returns a job. ErrNotFound if missing. Every method returning
	// jobs returns them with their RemoteIDs (sorted by ID), SearchText,
	// RepositoryID and ConcurrencyKeys (nil when there are none).
	GetJob(ctx context.Context, id domain.JobID) (Job, error)

	// UpdateJob is an optimistic, version-checked update: if the stored
	// job's Version equals j.Version, it writes every mutable field of j
	// (Status, Epoch, CancelRequestedAt, QueuedAt, DispatchedAt, StartedAt,
	// FinishedAt, Requeues, TimeLimit, Stats, Error, Warnings, ExitCode,
	// Output), sets
	// Version = j.Version + 1 and returns the stored job. The immutable
	// fields (ID, WorkerID, OwnerID, Kind, Definition, CreatedAt,
	// ScheduleID, RepositoryID, ConcurrencyKeys) are never written, nor are
	// RemoteIDs and SearchText.
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

	// ListQueuedJobs returns at most limit (negative: no limit) queued
	// jobs (any worker) with QueuedAt < before, by QueuedAt then ID
	// ascending: the candidates of the dispatch timeout. It is key-aware: a
	// job with ConcurrencyKeys is a candidate only if no older job (by
	// CreatedAt, then ID) sharing one of its keys is unfinished (queued,
	// dispatched or running) or finished at or after before. So a job
	// waiting behind an older sharer never times out, and its timeout
	// counts from when it became the head of its keys.
	ListQueuedJobs(ctx context.Context, before time.Time, limit int) ([]Job, error)

	// ListJobBlockers returns, for each job of ids that has concurrency
	// keys, the oldest unfinished (queued, dispatched or running) other job
	// sharing one of its keys that is older than it, by (CreatedAt, ID); a
	// job without such a blocker, without keys, or missing is omitted. One
	// query: the dispatcher's hot path (index on concurrency_keys).
	ListJobBlockers(ctx context.Context, ids []domain.JobID) (map[domain.JobID]domain.JobID, error)

	// ListWorkersWaitingOnKeys returns the distinct workers, by ID, of the
	// queued jobs whose ConcurrencyKeys share one of keys (they may start
	// now that a job holding keys finished).
	ListWorkersWaitingOnKeys(ctx context.Context, keys []string) ([]domain.WorkerID, error)
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

	// ListJobSamples returns samples of the job by Time ascending, and
	// total, the number of samples it has; one consistent read (SQL: one
	// statement). With maxPoints <= 0, or at most maxPoints samples, it
	// returns every sample. Otherwise it returns exactly maxPoints of the n
	// samples, evenly thinned: the one at rank (0-based, by Time) r is kept
	// if r == 0 or floor(r*(maxPoints-1)/(n-1)) > floor((r-1)*(maxPoints-1)/(n-1)),
	// which keeps the first and (for maxPoints >= 2) the last sample and
	// the first one of each of maxPoints equal spans of ranks. Both stores
	// apply this exact rule, so they return the same samples. An unknown
	// job has none (total 0, no error).
	ListJobSamples(ctx context.Context, id domain.JobID, maxPoints int) (samples []Sample, total int, err error)

	// PurgeJobSamples deletes every sample of at most maxJobs jobs whose
	// FinishedAt is before finishedBefore, that come after the cursor
	// after in (FinishedAt, ID) order and that still have samples, taken
	// in that order. It returns the number of samples it deleted and the
	// cursor of the last of those jobs (after itself when it deleted
	// nothing): 0 means no such job is left, so callers repeat it with the
	// returned cursor until it returns 0, and each run looks at every
	// finished job once rather than once per call. Unfinished jobs (zero
	// FinishedAt) are never touched; nor are the jobs themselves (their
	// final Stats stay). maxJobs must be positive. SQL: jobs_finished_idx
	// for the jobs from the cursor on and the samples' primary key, one
	// statement.
	PurgeJobSamples(ctx context.Context, finishedBefore time.Time, after PurgeCursor, maxJobs int) (deleted int, next PurgeCursor, err error)
}

// PurgeCursor is where SampleRepository.PurgeJobSamples continues: after
// the job that finished at FinishedAt with ID, in (FinishedAt, ID) order
// (IDs compare as Postgres orders UUIDs, byte by byte). The zero cursor
// starts before every job.
type PurgeCursor struct {
	FinishedAt time.Time
	ID         domain.JobID
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

// TimeLimits computes the effective time limit of a job, the one its
// worker is sent (*dispatch.Specs): its definition's max_runtime, else the
// controller's default (EffectiveTimeLimit).
type TimeLimits interface {
	// TimeLimitOf returns j's limit; zero for none.
	TimeLimitOf(j Job) time.Duration
}

// RemoteChecker checks the remotes a new job uses (*remotes.Service).
type RemoteChecker interface {
	// Usable returns the names of ids, which must all be live remotes of
	// owner: ErrNotFound if any is missing, deleted or another user's.
	Usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error)
}

// RepositoryChecker checks the restic repository a new job uses
// (*restic.Service).
type RepositoryChecker interface {
	// UsableRepository returns the name and the remote of a live
	// repository of owner: ErrNotFound if it is missing, deleted or
	// another user's. A repository whose data is being deleted is usable
	// only by its own deletion job (jobID equal to its DeletionJobID; jobID
	// is the job's preset ID, zero for API jobs and schedules); otherwise
	// it fails with an error matching ErrFailedPrecondition.
	UsableRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (name string, remoteID domain.RemoteID, err error)
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
