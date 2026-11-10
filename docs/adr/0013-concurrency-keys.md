# ADR 0013: Concurrency keys

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-09

## Context

Some jobs must not run at the same time. A restic `forget`, `prune` or
`check` takes an exclusive lock on its repository; a second exclusive
operation, or a backup that starts meanwhile, fails with
`REPOSITORY_LOCKED`. Deleting a repository's data must come after every
older job on it. The worker runners always said that serializing jobs per
repository is the controller's job, and nothing did it.

The jobs of one repository can be on different workers, created by
different schedules and by people, so neither a worker nor a schedule can
serialize them. The schedule overlap policy (ADR 0009) only orders the runs
of one schedule.

Constraints:

- Commands are derived from stored job state on every poll; the dispatcher
  keeps nothing durable of its own (ADR 0002).
- Two controllers, or two polls on different workers, may derive commands
  at the same moment.
- Job IDs are UUIDv7, but a scheduled job's ID is reserved before the job
  is created, so ID order is not creation order.
- The dispatch timeout fails a job that stays queued for 15 minutes.

## Decision

### Keys are set once, at creation

A job kind may name concurrency keys for a definition (`jobkind.Keyed`).
The restic kinds return `restic-repo:<repository id>`. The keys are
validated when the job is created (at most 8, each
`^[a-z0-9][a-z0-9:._-]{0,199}$`, sorted and deduplicated) and stored with
the job (`jobs.concurrency_keys`). They never change, so later edits of a
definition or a kind cannot change who waits for whom.

### Head of line per key, decided from stored state only

A queued job J is sent to its worker only if there is no **unfinished** job
O (queued, dispatched or running) that shares a key with J and is older:
`(O.created_at, O.id) < (J.created_at, J.id)`.

There is no lock table, lease or claim. The rule is a query over the jobs
table (`ListJobBlockers`, one call per derivation for the worker's queued
keyed jobs, served by a GIN index on the keys of the unfinished jobs only,
so a repository's job history does not slow it down), and it has these
properties:

- **Deterministic.** Among the unfinished jobs that share a key exactly
  one, the oldest, is the head.
- **Race-free.** Every controller and every poll computes the same head. An
  older job cannot appear later, because creation order is fixed.
- **Exclusive.** Only the head is ever sent, and it stays the head until it
  is finished. A job that was sent but waits in its worker's own queue
  still holds its keys.
- **Fair.** Jobs run in creation order per key. A job that is queued again
  (its worker restarted) keeps its creation time and so its place.
- **Deadlock-free.** With several keys per job, the oldest unfinished keyed
  job overall is the head of all its keys, so something can always start.
- **Restart-safe.** After a controller restart the same rows give the same
  heads.

### Waking the next job

When a job with keys reaches a terminal status, the job service looks up
the workers of the queued jobs that share its keys
(`ListWorkersWaitingOnKeys`) and wakes their long polls. A failure there is
only logged: active workers poll again within 25 s and passive loops within
15 s, and the gate is evaluated on every derivation.

### The dispatch timeout counts from becoming the head

A keyed job is a dispatch-timeout candidate only if no older sharer is
unfinished **and** none finished at or after the timeout's cutoff. A job
that waits behind others therefore never times out for waiting, and once it
is the head its worker gets the full 15 minutes.

### What users see

A queued job that waits has `Job.waiting_for_job_id`, the job it waits
for. The jobs list shows "Waiting for repository" with a link.

A queued head on an offline worker blocks the younger jobs on its
repository until it is dispatched, cancelled or fails with
`dispatch_timeout`. That is deliberate: skipping it would run jobs out of
order, for example a prune before the backup that was meant to precede it.
The head can be cancelled.

### Schedules

The overlap policy of a schedule and the keys work at different levels:

| Overlap | Effect of a job that waits on a key |
|---|---|
| `skip` | It is unfinished, so the schedule's next occurrence is missed with reason `overlap`. |
| `queue` | The next occurrence waits until the schedule has no unfinished job; its job may then wait on the key behind other schedules' jobs. |
| `allow` | Jobs are created on time and run one after another on the key. Use it only when every occurrence must run. |

### Probes take no keys

Probes (ADR 0012) read repositories with `--no-lock`. Browsing snapshots
while a backup runs is safe and must not wait.

### Transfers take no keys

Equality keys cannot express that `bucket/a` and `bucket/a/b` overlap, and
adding keys to transfers would change behaviour for existing users. The
overlap policy covers the common case. If it is needed later, an opt-in
`exclusive` flag on the transfer definition can produce a key
`rclone-dest:<remote>:<path>`; only the kind changes.

## Consequences

- Jobs on one repository run one at a time, oldest first, on any mix of
  workers, with no new state to keep consistent.
- The gate costs one indexed query per command derivation, and only for
  workers that have queued keyed jobs.
- Two edges remain, and restic's own locks are the safety net for both.
  A queued head that is cancelled while its `StartJob` is in flight may
  briefly run on its worker while the next head starts elsewhere; the
  orphan `CancelJob` stops it. A worker that is removed mid-run leaves its
  job interrupted while the restic process may still be alive. Exclusive
  operations then fail with `REPOSITORY_LOCKED`; concurrent backups are
  safe in restic.
- A stuck head delays everything behind it by up to the dispatch timeout.

## Alternatives considered

- **A lock table with leases.** It needs renewal, expiry and recovery after
  crashes, and a second source of truth next to the job status. The jobs
  table already says who holds a key.
- **Serializing on the worker.** Jobs of one repository can be on several
  workers.
- **Relying on restic's locks and retrying.** Exclusive operations would
  fail and be retried in an arbitrary order, and failures would notify
  people.
