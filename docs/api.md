# Scripting the API

Everything the UI does goes through the Connect API
([`proto/syncr/v1/`](../proto/syncr/v1/)). For scripts, create a personal API
token under **Settings, API tokens** (it acts as you, with your role) and
send it as a bearer token:

```sh
export SYNCR_TOKEN='syncr_pat_...'   # shown once when you create it
curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/syncr.v1.WorkerAdminService/ListWorkers
# {"workers":[{"id":"01a1a7c0-...", "name":"nas", "mode":"WORKER_MODE_ACTIVE", "status":"WORKER_STATUS_ONLINE", ...}]}

curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"workerId":"<worker id>","definition":{"rcloneTransfer":{"mode":"RCLONE_TRANSFER_MODE_COPY",
       "source":{"path":"/srv/data/photos"},"destination":{"remoteId":"<remote id>","path":"photos"}}}}' \
  http://127.0.0.1:8080/syncr.v1.JobService/RunJob
# {"job":{"id":"01a1a7c0-...", "status":"JOB_STATUS_QUEUED", ...}}
```

A definition may name hooks of the worker to run around the job
(`"hooks":{"before":["dump-db"],"after":["start-app"]}` next to the
operation; the lists are `before`, `onSuccess`, `onFailure` and `after`).
A worker's hooks are in `ListWorkers` under `info.hooks`, and how they went
in a job's `hookRuns` ([hooks](hooks.md)).

A [two-way sync](bisync.md) is the kind `rcloneBisync`, with two locations
and typed options (`RcloneBisyncOptions` in
[`link.proto`](../proto/syncr/v1/link.proto)):

```sh
curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"workerId":"<worker id>","definition":{"rcloneBisync":{
       "path1":{"path":"/home/ada/Shared"},"path2":{"remoteId":"<remote id>","path":"shared"},
       "options":{"maxDeletePercent":30,"filters":[{"action":"FILTER_ACTION_EXCLUDE","pattern":"*.tmp"}]}}}}' \
  http://127.0.0.1:8080/syncr.v1.JobService/RunJob
```

- `resync` and `force` (next to `path1` and `path2`) are **one-shot**: set
  them in `RunJob` for a single run (what the UI's **Resync now** and
  **Run once anyway** do). Schedules refuse them, with field errors on
  `definition.rclone_bisync.resync` and `.force`.
- A schedule of this kind must target a worker: `target.workerLabel` is
  refused with a field error on `target.worker_label`.
- The worker may fail it with three codes of its own in `error.code`:
  `JOB_ERROR_CODE_RESYNC_REQUIRED` (29: only a run with `resync` helps,
  after changed filters, an empty worker folder on the pair's first run, or
  a critical error that left no earlier state), `JOB_ERROR_CODE_SAFETY_ABORT`
  (30, what `force` overrides) and `JOB_ERROR_CODE_SIDE_UNAVAILABLE` (31).
  Other critical errors, after which the next run goes on from the pair's
  last good state, are `JOB_ERROR_CODE_EXECUTION_FAILED` with the cause to
  fix. A worker without the `rclone.bisync` feature (older than 2.1.0)
  fails it with `JOB_ERROR_CODE_WORKER_OUTDATED`.
- A successful run's `output.rcloneBisync` says whether it was a resync and
  why (`resyncReason`), and counts the conflicts (`conflicts`, with the
  first 20 in `conflictPaths`).

A location is a path on one of your remotes (`remoteId`, path relative to its
root) or, without `remoteId`, an absolute path on the worker inside its
`local_roots`. `JobService.WatchJob` streams status changes and live
progress (a Connect server stream; use a Connect or gRPC client rather than
plain `curl`). Calls with the browser's session cookie need the
`X-Syncr-CSRF: 1` header; bearer calls do not. API tokens cannot create or
delete tokens or change the password, and an admin's token cannot create
users or reset passwords: each of these needs a signed-in session.
