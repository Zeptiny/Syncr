# ADR 0022: Metrics and a fuller dashboard

- Status: accepted (v2.3.0)
- Date: 2026-11-21

## Context

The dashboard counted finished jobs per hour or day by outcome, with the
bytes and files they moved, and listed the next runs, the workers and the
latest problems. The jobs table already stored each job's kind, worker,
error code and controller and worker times; none of it reached the
dashboard. People running Syncr next to Prometheus also asked for
`/metrics`, to alert on a backup that stopped succeeding without opening
the UI.

What shaped the design:

- **Every user may run jobs on every worker, and schedules and restic
  repositories are owner-only, admins included** (ADR 0006, 0009, 0011).
  Admins see every user's jobs (`all_users`), never another user's
  schedules or repositories.
- **One controller** (ADR 0015), Postgres for durable state; jobs are never
  deleted (only their samples, ADR 0014).
- **Few dependencies**: the module list is short and pinned.

## Decision

### `/metrics` is the dashboard for machines

`GET /metrics` on the controller's listener serves the Prometheus text
format, or OpenMetrics when asked, through `prometheus/client_golang`
(`promhttp`). It is the one new direct dependency (five small indirect
ones come with it): hand-writing the format would save them but lose
content negotiation, compression and the Go and process collectors, and a
format bug would only show in a scraper.

A scrape authenticates with a **personal API token**, through the API's
Authenticator, and nothing else: no new secret to configure, a token per
scraper that can be revoked, and no ambient credential, so a session
cookie (even with the CSRF header) is refused. It sees what the token's
user sees on the dashboard: every worker; the user's jobs, or every
user's for an admin (an operator's scrape is about the installation, so
`all_users` is implied); the user's own schedules and repositories. Go
runtime and process metrics are for admins only.

Labels are names (`worker`, `schedule`, `repository`), unique per owner
among live objects; series are merged by labels, so a removed worker and
a new one of the same name add up instead of failing the scrape. No label
names a user.

### Counters come from the database

The job counters (finished jobs by kind, worker, status and error code,
bytes, files, the run-time histogram and the queue-wait summary) are
counted from the stored jobs on every scrape, not kept in memory. They
survive controller restarts, include jobs that ended while nothing
scraped, and need no hook into the job lifecycle. The cost is a count
over every finished job per scrape; migration 00013 adds
`jobs_totals_idx`, keyed `(owner_id, finished_at)` like the
`jobs_owner_finished_idx` it replaces and including every column the
count reads, so Postgres answers with an index-only scan. On 200,000 jobs
it read about 4,400 pages instead of the table's 28,600; one owner's
count took 25 ms.

Run time is `finished_at - started_at` (the worker's clock, as on the job
pages) and queue wait `dispatched_at - created_at` (the controller's), in
whole microseconds in both stores; jobs without one are left out of its
count and sum. The sums stay whole microseconds (`int64`) up to the
exposition and the dashboard's protobuf durations: summed over every job
ever stored, a `time.Duration` would overflow after 292 years of run time. Run-time buckets are computed in the query (the number of
bounds below the run time, Prometheus's `le` semantics), so the histogram
needs no per-job rows.

Schedule gauges use the newest run (with its job's status) and the newest
succeeded job the schedule launched (`jobs_schedule_idx`, newest first,
up to the first success). The newest run's state is an OpenMetrics state
set (`syncr_schedule_last_run_status`: one series per state).

### The dashboard reads the same aggregates

`GetDashboard` adds, over its range: run time and queue wait per bucket
and in total; the jobs by kind and by worker (`ActivityRepository.JobTotals`
with the range's bounds); failed and interrupted jobs by error code; the
caller's schedules with their newest run and last success, those whose
newest run did not succeed first; and the caller's restic repositories
with what their successful backups added. Repository growth is read from
the backups' outputs (`data_added_packed`, else `data_added`, and
`total_bytes_processed`), at most 2,000 backups newest first. Prune
reports no freed bytes and the controller records no repository size over
time, so the dashboard says what backups wrote, not how big a repository
is.

The stat band shows six figures (success rate, jobs run, transferred,
average run time, average queue wait, running now); the missed runs moved
to the schedules panel. Run time and queue wait are two small charts with
one series each, not one chart with two scales.

## Consequences

- One more index on `jobs` (narrow, partial, replacing one), and a count
  over the history per scrape that grows with it. Scrapes every 30 to 60 s
  are cheap; the docs say so.
- Renaming a worker, schedule or repository starts new series.
- An admin who wants every user's schedules in Prometheus scrapes with
  each owner's token; schedules stay private.
- Repository growth is a lower bound when a range holds more than 2,000
  backups (`truncated`), and ignores pruning.
