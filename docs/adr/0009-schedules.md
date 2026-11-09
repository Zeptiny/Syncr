# ADR 0009: Schedules

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-08

## Context

Milestone 4 lets a user run a job definition on a cron schedule. Syncr's
workers are often desktops and laptops that sleep, so "run it at 02:30"
has to mean something sensible when nobody is there at 02:30. The design
has to handle:

- **Time.** Users think in local wall time ("nightly at 02:30 in Berlin").
  Daylight saving time makes some wall times not exist (spring forward) and
  others exist twice (fall back). A nightly backup must not be silently
  skipped, nor run twice.
- **Offline workers.** The dispatcher fails a job that stays queued for 15
  minutes (`dispatch_timeout`), so the scheduler must not queue jobs on
  offline workers: it holds the run itself until the worker is back, within
  a deadline.
- **Restarts and races.** The controller can crash between any two steps,
  and a second controller may run against the same database. A scheduled
  job must never run twice for one occurrence.
- **Layering.** Use cases cannot import each other (depguard), so the loop
  that combines schedules, jobs, workers and users lives in the controller
  layer, like the dispatcher.

## Decision

### Cron expressions: `internal/cronexpr`

- Five fields or the descriptors `@yearly`, `@annually`, `@monthly`,
  `@weekly`, `@daily`, `@midnight` and `@hourly`. Seconds, `@every`,
  `TZ=`/`CRON_TZ=` prefixes and expressions that never match (February 30)
  are rejected, as are the time zones `""` and `Local`: the zone is a
  separate IANA name, so a schedule never depends on the controller host's
  zone.
- `github.com/robfig/cron/v3` parses (only). It has been unmaintained since
  2020, but its parser's API is frozen and replacing it touches only this
  package.
- **Occurrences are local wall times** matching every field, with cron's
  day-of-month/day-of-week OR rule (robfig's star-bit semantics: when both
  day fields are restricted, either matches).
- **One rule for DST:** a wall time runs at the first instant at which the
  zone's clock shows it or a later time. So a wall time in a spring-forward
  gap runs at the transition (Berlin 02:30 runs at 03:00 CEST; several gap
  occurrences collapse into one run), and a repeated wall time runs once, at
  its first pass (`*/15` runs only during the first pass of the repeated
  hour). This is cronie's behaviour.
- **Computation** is our own, not robfig's (which steps instants by hours
  and so can skip gap times or run twice): a field-wise calendar iteration
  over months, days, hours and minutes on "naive" wall times, then each
  matching wall time is resolved by walking the zone's periods with
  `time.Time.ZoneBounds`. The walk is exact for any transition, tested on
  Berlin, New York, Australia/Lord_Howe (30-minute shifts), Sao Paulo 2018
  (midnight did not exist), Pacific/Apia 2011 (a skipped day) and Kolkata
  (+05:30).
- `Next` looks five years ahead (an expression with no occurrence then
  stops), `NextN` previews at most 20, and `Latest(after, until, max)`
  returns the latest occurrence of an interval and how many it holds, for
  coalescing.
- The controller binary embeds `time/tzdata`, so minimal hosts resolve IANA
  names.

### Schedules, runs and policies: `internal/app/schedules`

- A schedule stores a checked, secret-free `JobDefinition` of any registered
  job kind (protojson, remotes by ID), exactly as `RunJob` takes it, plus
  its target (one worker, or any worker with an admin-managed label), a
  missed-run policy, an overlap policy and contacts to notify.
- Schedules are **owner-only, admins included**, like remotes: another
  user's schedule is NotFound, and linking another user's remote or contact
  is NotFound. Admins see the resulting jobs through `ListJobs.all_users`.
- Each occurrence becomes a **run**: `pending` (waiting for its worker or an
  active run), `launching` (its job ID reserved), then `launched`, `missed`,
  `failed` or `cancelled`. **A schedule has at most one pending run** (a
  unique partial index; migration 00005 narrowed it from one open run): a
  later occurrence due before the run's deadline is merged into it
  (`coalesced + 1`), so a desktop that slept through three nights runs
  once when it wakes (anacron-style, within the deadline). The deadline is
  fixed when the run opens and a merge never moves it: an occurrence due
  at or after it supersedes the run, which is missed (and notified) with
  the reason the scheduler would give at its deadline, and opens a run of
  its own. Otherwise a run would absorb every next occurrence and never be
  missed, e.g. with "until the next occurrence" or a `max_delay` longer
  than the period and a worker that stays offline. A manual request that
  merges into the pending run extends its deadline to the request's own
  (it asked to run now). A launching run absorbs nothing, since its job is
  already decided: an occurrence due while a run launches opens a pending
  run of its own, which the overlap policy then records missed (skip, as
  the launching run is active), holds (queue) or launches alongside
  (allow).
- **Fire** (pure, `PlanFire`): when `next_run_at <= now`, one run for the
  latest occurrence in `[next_run_at, now]`, the earlier ones counted in
  `coalesced`; `next_run_at` becomes the next occurrence after now.
  Downtime never replays occurrences one by one.
- **Deadline**: `run_late` waits `max_delay` (at most 7 days), or until the
  next occurrence when it is 0 (so each next occurrence supersedes a run
  still waiting); `skip` allows a 5-minute grace. A manual run counts from
  the request.
- **Overlap**: `skip` (default) records the occurrence as `missed`
  (`overlap`) while a run is active (launching, or its job queued,
  dispatched or running); `queue` waits for it; `allow` runs alongside.
  `RunScheduleNow` on a skip schedule with an active run is
  FailedPrecondition.
- **Auto-pause** on permanent launch failures (worker removed, definition no
  longer valid, a remote gone, owner disabled): the run fails, the schedule
  is disabled with `paused_reason`, shown in the UI. A label without workers
  fails the run but does not pause (an admin may add the label). A run that
  failed because its owner is disabled is not notified.
- Pausing needs no version and cancels the open run; resuming recomputes the
  next run from now. Edits recompute it only when the expression, zone or
  enabled state changed, so an edit never rewinds or skips a fire.

### The scheduler: `internal/controller/scheduler`

A single loop, ticking every `schedules.tick` (10 s) and woken when a
schedule is saved, a run added or a job finished. One pass fires the due
schedules, then decides each open run, oldest first: past its deadline it
is missed; behind an active run it waits; a disabled owner or an invalid
definition fails it; then the target (a removed worker fails it, an offline
one keeps it waiting; by label, the least busy online worker with the label
that reports the job kind is chosen, fewest unfinished jobs first, then by
name).

**No double runs**, across restarts and two controllers, without
application locks or leader election:

1. A fire is a compare-and-swap on `next_run_at` in one transaction with
   the run's merge or insert.
2. The one-pending-run index makes a second pending run impossible.
3. The launch has two phases: `pending -> launching` is a compare-and-swap
   that reserves a job ID on the run, then `jobs.Create` with that preset
   ID (idempotent: AlreadyExists counts as success), then `launching ->
   launched`.
4. A crash between any two steps is completed from the stored state: a run
   found `launching` is created with its reserved ID on the next pass,
   whatever its deadline (the launch was decided in time).

`schedule_runs.job_id` therefore has no foreign key: the ID exists before
the job. Deleting a schedule cascades to its runs and links; its jobs stay,
with `schedule_id` cleared.

### Postgres

`schedules`, `schedule_remotes`, `schedule_contacts` and `schedule_runs`
(migration 00003; 00005 makes the one-open-run index one pending run).
Every port method is one statement or one transaction:
`FireSchedule` is the CAS plus the merge or insert; `UpdateRun` is a CAS on
the status plus the optional auto-pause; `UpdateSchedule` locks the row
(`FOR UPDATE`) to decide the version check and whether the open runs must be
cancelled. Every transaction that writes a schedule's runs locks the
schedule row first, then the runs (`UpdateRun` and `AddRun` lock the
schedule before touching a run), so an auto-pause racing a user's pause
cannot deadlock. A deadlock or serialization failure that still happens
(e.g. with other tables) maps to `ErrConflict`, a lost race the caller
retries. The races (fires, open runs, launch claims, the lock order) are
covered by integration tests across two connection pools.

## Consequences

- Occurrences are exact and predictable in any zone; previews come from the
  server (`PreviewSchedule`), so the UI never re-implements the rules.
- A sleeping desktop runs its backup once when it wakes, within the
  deadline; otherwise the run is missed and notified.
- A worker that went to sleep less than 45 s before an occurrence still
  looks online: its job is queued and fails with `dispatch_timeout` after 15
  minutes, which counts as a failed run. Waiting for "seen within two
  heartbeats" is left to Milestone 6.
- A dispatcher-level concurrency key (restic's per-repository lock, also for
  manual jobs) is deferred to Milestone 5; within one schedule the overlap
  policy and the one-open-run rule serialize runs.
- The scheduler is safe with two controllers, but the rest of the
  controller (the link hub, rate limits) is still in-process (a Milestone 6
  gap).
