# ADR 0014: Job time limits and sample retention

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-11

## Context

Nothing stopped a job that ran too long. A hung rclone or restic process
kept its worker slot and, for restic, its repository's concurrency key
(ADR 0013) until someone cancelled it, so every later job on the
repository waited behind it.

Nothing was ever deleted either. A running job stores a progress sample
every 5 seconds, about 17,000 rows per day of job runtime, and the job's
chart read all of them.

Constraints:

- Workers built before Milestone 6 ignore spec fields they do not know.
- A cancelled job does not notify anyone, and a job that ends while its
  worker has no connection is only learned about later.
- The restic snapshot cache decides staleness from a count of ended jobs
  (ADR 0011).

## Decision

### A hard time limit, enforced by the worker

`JobDefinition.max_runtime` (1 minute to 366 days) applies to every kind;
schedules store a definition, so they get it too. The controller has a
default, `jobs.default_max_runtime`, which is 0 (no limit) unless the
operator sets one. The effective limit is the definition's value, or else
the default (`jobs.EffectiveTimeLimit`, the one place the rule lives).

Kinds that only the system starts (`jobkind.SystemOnly`, today
`restic_delete_data`) never get the default. Users cannot edit those
definitions. A limit meant for user jobs could stop the deletion of a large
repository halfway.

`dispatch.Specs.Build` writes the effective limit into
`JobSpec.max_runtime`. When the worker accepts the job, the jobs service
stores the same value on the job (`jobs.time_limit_ns`, computed by
`dispatch.Specs.TimeLimitOf`), and the API shows it as `Job.time_limit`. A
finished job therefore keeps the limit it ran with when
`jobs.default_max_runtime` changes later. A queued job shows the limit it
would be sent now (`dispatch.Specs.TimeLimit`), and a job that ended before
a worker accepted it shows none.

The worker counts the limit from the start of the job's process, on its
monotonic clock. Waiting in the queue or behind a concurrency key does not
use it up. At the limit it stops the process as it stops a cancelled job:
SIGINT, the grace period, then the whole process tree (ADR 0003). It
reports `FAILED` with `JOB_ERROR_CODE_TIME_LIMIT` and a message that names
the limit.

The job ends **failed** (`time_limit`), not cancelled. So it notifies its
contacts, fails its schedule's run and releases its concurrency keys, and
the next job on the repository starts, like any other failure. The
controller records `TIME_LIMIT` as failed whatever status came with it.

### A feature gate instead of trust

Workers that enforce the limit report `link.FeatureJobTimeLimit`
(`job.time_limit`). A job with a limit for a worker without the feature
fails with `worker_outdated` ("must be upgraded to run jobs with a time
limit") rather than running without a limit. Jobs without a limit still
run on such workers. The scheduler's choice of a worker for a label does
not look at the feature: no released worker lacks it.

### rclone's `max_duration` stays

rclone's own option only stops starting new transfers and lets the running
ones finish. The two are documented side by side.

### Samples are kept for a while; jobs are kept

`jobs.sample_retention` defaults to 720h (30 days), with a minimum of 1h;
0 keeps everything. `jobs.Service.PurgeSamples`, called hourly by the
controller, deletes the samples of the jobs that finished before
`now - retention`. It works in batches of 50 jobs per statement, oldest
finished first, and repeats until a batch deletes nothing. Each batch goes
on after the last job of the one before (a cursor on `(finished_at, id)`),
and each run after the last job of the run before, so the jobs already
purged are not walked again. Once a day a run starts from the oldest job
again: a worker may report a job's end late, with its own clock, after the
purge has passed that time. Unfinished jobs are never touched. A job's final counters live on the job row, so its totals
stay after its chart is gone; the UI says so for a finished job without
samples.

Charts are thinned at read time: `ListJobSamples` returns at most
`max_points` samples (default 1000, at most 5000), evenly spread, always
with the first and the last, plus the total. Both stores apply the same
rule, pinned by the storetest suite.

### Why job rows and schedule runs are not purged

The snapshot cache of a restic repository is stale when the repository's
change mark differs from the one stored with the cache. The mark counts the
ended jobs on the repository that may have changed its snapshots, with the
newest finish time among them (`restic.ChangeMark`). Deleting old jobs
lowers the count. Later jobs can bring it back to the stored value, and the
newest finish time catches that only if one of them finished later by its
worker's clock. Worker clocks are not comparable, so a purge could hide a
change and serve a stale snapshot list as current.

Purging jobs first needs a durable per-repository change counter,
incremented in the same statement that ends the job. Schedule runs refer to
their jobs and would go with them. Sizes do not press: an hourly schedule
adds about 8,800 job rows a year, and PR #7 measured the hot queries at
400,000 jobs.

## Consequences

- A hang now ends at the job's time limit, if it has one, and the next job
  on its repository starts.
- A limit set too low stops a legitimate run. The limit is opt-in, the
  message names it, and the job ends failed, so its contacts hear about it.
- A worker older than Milestone 6 cannot run jobs with a limit. They fail
  at once with a message that asks for an upgrade.
- `job_samples` stays bounded by the runtime of the last 30 days of jobs.
- The purge uses the worker's `FinishedAt`. A worker whose clock is far
  behind loses its jobs' charts that much earlier.
- The daily run walks the finished jobs from the oldest one
  (`jobs_finished_idx`), with one primary-key check per job for remaining
  samples, so its cost grows with the number of jobs kept. Measured on
  Postgres 17 with 400,000 finished jobs (one every 157 seconds over two
  years, the last 30 days' 16,500 with 20 samples each): that walk takes
  about 0.7 seconds; a batch that goes on from a cursor takes 3
  milliseconds. Before the cursor, every batch of a run walked from the
  oldest job again, about 0.7 seconds each. The hourly runs in between
  start from the last cursor and cost milliseconds. The cursor is kept in
  memory, so a restart costs one walk.

## Alternatives considered

- **Stopping jobs from the controller.** It needs a durable "stop requested,
  and why" state. The result would be `cancelled`, which does not notify, and
  a worker without a connection would never get the stop.
- **Purging samples by their own time.** It would cut the chart of a
  long-running job at its start while the job still runs.
- **Deleting old jobs with their samples.** See above: it can hide a
  change from the snapshot cache.
