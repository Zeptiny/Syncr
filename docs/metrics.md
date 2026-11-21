# Metrics for Prometheus

The controller serves its state at `GET /metrics` in the Prometheus text
format (and OpenMetrics, when the scraper asks for it), so Prometheus,
VictoriaMetrics, Grafana Alloy, the OpenTelemetry Collector or any other
compatible scraper can chart it and alert on it: workers that went offline,
jobs that fail, a backup that has not succeeded for a day, a queue that
does not drain.

It is on the controller's usual address; there is nothing to configure.

## Scraping

A scrape signs in with a **personal API token**, like a script calling the
API ([scripting the API](api.md)). Create one under **Settings, API
tokens** (for a dedicated account, sign in as it first; see
[what a scrape shows](#what-a-scrape-shows)), save it in a file only
Prometheus can read, and point the scrape job at it:

```yaml
scrape_configs:
  - job_name: syncr
    scheme: https
    metrics_path: /metrics
    scrape_interval: 60s
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/syncr-token   # syncr_pat_...
    static_configs:
      - targets: ["syncr.example.com"]
```

Without a token, or with a session cookie, the controller answers
`401 Unauthorized`. A token stops working when it expires, is deleted, or
its user is disabled; the scrape then fails with 401, which Prometheus
shows as the target being down (`up == 0`).

Behind a reverse proxy, pass `/metrics` through like the API (or keep it
to your internal network); it needs no long timeouts.

### What a scrape shows

A scrape sees what the token's user sees on the dashboard:

| | A user's token | An admin's token |
|---|---|---|
| Workers | every worker | every worker |
| Jobs | the user's own | **every user's** |
| Schedules, restic repositories | the user's own | the admin's own |
| Go runtime and process (`go_*`, `process_*`) | no | yes |

Schedules and repositories are private to their owner, admins included,
as everywhere in Syncr. To watch every user's schedules, each user scrapes
with their own token, or the schedules belong to the account Prometheus
uses.

## Metrics

Labels name things by name: `worker` is the worker's name, `schedule` the
schedule's and `repository` the repository's. Renaming one starts new
series. Jobs of a removed worker count under its name, together with a
newer worker of the same name.

### Controller and workers

| Metric | Type | Labels | |
|---|---|---|---|
| `syncr_build_info` | gauge | `version`, `commit`, `go_version` | always 1 |
| `syncr_workers` | gauge | `status` | workers that are `online`, `offline` or `pending` (enrolled, never connected) |
| `syncr_worker_up` | gauge | `worker` | 1 while the worker is online |
| `syncr_worker_last_seen_timestamp_seconds` | gauge | `worker` | when the controller last heard from it |
| `syncr_worker_info` | gauge | `worker`, `mode`, `version`, `os`, `arch` | always 1 |
| `syncr_worker_outdated` | gauge | `worker` | 1 when it should be upgraded to the controller's version |

### Jobs

| Metric | Type | Labels | |
|---|---|---|---|
| `syncr_jobs` | gauge | `status`, `kind`, `worker` | jobs `queued`, `dispatched` (accepted, not started) or `running` now |
| `syncr_jobs_oldest_queued_timestamp_seconds` | gauge | `kind`, `worker` | when the longest-waiting queued job entered the queue |
| `syncr_jobs_finished_total` | counter | `kind`, `worker`, `status`, `error_code` | finished jobs by final status (`succeeded`, `failed`, `interrupted`, `cancelled`) and error code (empty without one) |
| `syncr_job_transferred_bytes_total` | counter | `kind`, `worker` | bytes finished jobs transferred |
| `syncr_job_transferred_files_total` | counter | `kind`, `worker` | files finished jobs transferred |
| `syncr_job_run_duration_seconds` | histogram | `kind`, `worker` | how long finished jobs ran, start to end (buckets from 1 s to 24 h) |
| `syncr_job_queue_wait_seconds` | summary | `kind`, `worker` | how long finished jobs waited from creation until a worker accepted them (`_sum` and `_count`) |

`kind` is the job's kind: `rclone_transfer`, `rclone_bisync`,
`restic_backup`, `restic_forget`, `restic_prune`, `restic_check`,
`restic_restore`, `restic_delete_data`. `error_code` is the lower-case
code the job pages show in words, for example `execution_failed`,
`worker_restarted`, `hook_failed`, `time_limit` or `resync_required`.

The counters are read from the database: they count **every finished job
ever stored**, so they do not restart with the controller. `rate()` and
`increase()` work on them as on any counter.

### Schedules

| Metric | Type | Labels | |
|---|---|---|---|
| `syncr_schedule_info` | gauge | `schedule`, `kind` | always 1 |
| `syncr_schedule_enabled` | gauge | `schedule` | 1 enabled, 0 paused |
| `syncr_schedule_next_run_timestamp_seconds` | gauge | `schedule` | when it runs next (absent while paused) |
| `syncr_schedule_last_run_timestamp_seconds` | gauge | `schedule` | when its newest run was due |
| `syncr_schedule_last_success_timestamp_seconds` | gauge | `schedule` | when the newest job it launched that succeeded finished (absent if none did) |
| `syncr_schedule_last_run_status` | gauge | `schedule`, `status` | how the newest run went: one series per state, 1 for the current one |

The states of `syncr_schedule_last_run_status`: `active` (waiting or
running), `succeeded`, `failed`, `interrupted`, `cancelled` (the job, or
the run because the schedule was paused), `missed` (could not start in
time) and `not_started` (could not start at all, for example its remote
was deleted). A schedule that never ran has none.

### Restic repositories

| Metric | Type | Labels | |
|---|---|---|---|
| `syncr_restic_repository_snapshots` | gauge | `repository` | snapshots, as of the snapshot list's last refresh |
| `syncr_restic_repository_snapshots_refreshed_timestamp_seconds` | gauge | `repository` | when that list was refreshed (opening the repository refreshes it) |
| `syncr_restic_repository_latest_snapshot_timestamp_seconds` | gauge | `repository` | when its newest snapshot was taken |
| `syncr_restic_repository_latest_snapshot_size_bytes` | gauge | `repository` | the size of what that snapshot backed up |
| `syncr_restic_repository_last_backup_timestamp_seconds` | gauge | `repository` | when its newest backup job finished |
| `syncr_restic_repository_last_backup_succeeded` | gauge | `repository` | 1 if that job succeeded (absent while it runs) |

## Example alerts

```yaml
groups:
  - name: syncr
    rules:
      - alert: SyncrControllerDown
        expr: up{job="syncr"} == 0
        for: 5m
      - alert: SyncrWorkerOffline
        expr: syncr_worker_up == 0 and on(worker) syncr_worker_last_seen_timestamp_seconds > 0
        for: 15m
      - alert: SyncrBackupStale
        # No success for 26 hours on a daily schedule that is enabled.
        expr: |
          (time() - syncr_schedule_last_success_timestamp_seconds) > 26 * 3600
          and on(schedule) syncr_schedule_enabled == 1
      - alert: SyncrScheduleFailing
        expr: syncr_schedule_last_run_status{status=~"failed|interrupted|missed|not_started"} == 1
      - alert: SyncrJobsFailing
        # More than a quarter of the jobs that finished in the last hour failed.
        expr: |
          sum(increase(syncr_jobs_finished_total{status=~"failed|interrupted"}[1h]))
            / sum(increase(syncr_jobs_finished_total[1h])) > 0.25
      - alert: SyncrQueueStuck
        expr: time() - syncr_jobs_oldest_queued_timestamp_seconds > 3600
```

Useful queries for a dashboard: jobs per hour by status,
`sum by (status) (increase(syncr_jobs_finished_total[1h]))`; data
transferred per day, `sum(increase(syncr_job_transferred_bytes_total[1d]))`;
the 95th percentile run time by kind,
`histogram_quantile(0.95, sum by (kind, le) (rate(syncr_job_run_duration_seconds_bucket[1d])))`;
the average queue wait,
`rate(syncr_job_queue_wait_seconds_sum[1h]) / rate(syncr_job_queue_wait_seconds_count[1h])`.

## Cost

Each scrape reads the database: the workers, the unfinished jobs, the
schedules and repositories of the token's user, and one count over every
finished job. That count reads a narrow index (`jobs_totals_idx`, from
migration 00013) instead of the jobs themselves, so it stays quick with
hundreds of thousands of jobs; it still grows with the history, so scrape
every 30 to 60 seconds rather than every few. Design notes:
[ADR 0022](adr/0022-metrics.md).
