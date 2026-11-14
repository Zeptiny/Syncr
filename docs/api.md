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

A [backup of a folder of a remote](restic.md#back-up-a-folder-of-a-remote)
is a `resticBackup` with `remoteSource` instead of `paths`
(`ResticRemoteSourceDefinition` in
[`api.proto`](../proto/syncr/v1/api.proto)):

```sh
curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"workerId":"<worker id>","definition":{"resticBackup":{"repositoryId":"<repository id>",
       "remoteSource":{"remoteId":"<remote id>","path":"Photos","method":"RESTIC_REMOTE_SOURCE_METHOD_MIRROR",
       "filters":[{"action":"FILTER_ACTION_EXCLUDE","pattern":".thumbnails/**"}]}}}}' \
  http://127.0.0.1:8080/syncr.v1.JobService/RunJob
```

- `method` is `RESTIC_REMOTE_SOURCE_METHOD_MIRROR` or `…_MOUNT`.
  `discardMirror`, `transfers` and `fastList` are for the mirror only;
  `allowEmpty` lets an empty folder pass. `excludes`, `oneFileSystem` and
  `useFsSnapshot` are refused with a remote source, and at most 31 tags of
  your own (Syncr adds `syncr-remote:<remote id>`).
- What a worker can do is in `ListWorkers` under `info.remoteSources`
  (`mirror`, `mount`, `mirrorReason`, `mountReason`; unset for a worker
  that never reported it). `RunJob` refuses a worker that cannot use the
  method, on `worker_id`; a job that reaches such a worker anyway fails
  with `JOB_ERROR_CODE_TOOL_UNAVAILABLE` and its reason. A worker without
  the `restic.remote_source` feature (older than 2.2.0) fails it with
  `JOB_ERROR_CODE_WORKER_OUTDATED`.
- A schedule of it must target a worker: `target.workerLabel` is refused
  with a field error on `target.worker_label`.
- An empty folder fails with `JOB_ERROR_CODE_SOURCE_EMPTY` (32) unless
  `allowEmpty` is set; then the job succeeds with an empty `snapshotId`
  and a warning (restic makes no empty snapshot).
- Live progress (`WatchJob`) carries `phase`: `mirror` (rclone's
  counters) then `backup` (restic's, from zero again). Stored samples
  (`ListJobSamples`) have no phase. A successful run's
  `output.resticBackup.remoteSource` holds the mirror's counters.

The [file browser](browsing.md)'s scans are calls of their own. All of
them go through a worker and answer within the request:

| Call | What it does |
|---|---|
| `ProbeService/MeasureWorkerFolder` | folder sizes of a worker's folder (`workerId`, `path`) |
| `ProbeService/MeasureRemoteFolder` | folder sizes of a folder of one of your remotes (`remoteId`, `path`, optional `workerId`) |
| `ProbeService/GetRemoteUsage` | a remote's usage, as `rclone about` reports it |
| `ResticService/MeasureSnapshotFolder` | folder sizes of a snapshot's folder |
| `ResticService/FindInRepository` | `restic find` over all snapshots or some (`patterns`, `ignoreCase`, `snapshotIds`) |
| `ResticService/CompareSnapshots` | `restic diff` of two snapshots, optionally of one folder (`fromSnapshotId`, `toSnapshotId`, `path`) |

```sh
curl -sS -H "Authorization: Bearer $SYNCR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"repositoryId":"<repository id>","patterns":["*.pdf"],"ignoreCase":true}' \
  http://127.0.0.1:8080/syncr.v1.ResticService/FindInRepository
# {"matches":[{"snapshotId":"5f1c...","entry":{"name":"lease.pdf","path":"/srv/docs/lease.pdf",...}}],
#  "complete":true,"snapshotsSearched":12,"worker":{...}}
```

- The five scans accept POST only (opening a URL cannot start one), are
  bounded by `probe.scan_timeout` and run one at a time per worker: a
  second one gets `unavailable` ("already measuring or searching"); a
  worker whose probe slots are all taken answers `unavailable` ("busy
  with other requests"). The web UI keeps a search or comparison in its
  page's address, but opening such an address (a link, a reload) only
  fills in the form: the scan runs when you press **Search** or
  **Compare**, or when you come from a page of the app that started it.
- **A partial answer is a success.** `complete: false` means the scan
  stopped at its time limit (every number is a lower bound; a comparison
  then has no `stats`). `truncated: true` means a limit was reached:
  1,000 matches, 2,000 changes (the `stats` still cover every change), or
  more child folders than listed (`childrenTruncated`). A measurement's
  `unreadable` counts entries the worker could not read.
- Measurements list the child folders largest first. Search matches come
  newest snapshot first, with full snapshot IDs. Change paths are full
  snapshot paths; folders end in `/`.
- A remote on a worker's disk needs `workerId`; `GetRemoteUsage` then
  answers `usage.unsupported`, like a backend that cannot tell.

Copy and move from the browser are ordinary `RunJob` calls with an
`rcloneTransfer` definition whose `filters` select the items; restoring
selected items back to their place is a `resticRestore` with both
`subfolder` and `paths` (each path inside the subfolder), which needs a
worker with the `restic.restore_subfolder_paths` feature (2.1 or later;
older workers fail it with `JOB_ERROR_CODE_WORKER_OUTDATED`).

A location is a path on one of your remotes (`remoteId`, path relative to its
root) or, without `remoteId`, an absolute path on the worker inside its
`local_roots`. `JobService.WatchJob` streams status changes and live
progress (a Connect server stream; use a Connect or gRPC client rather than
plain `curl`). Calls with the browser's session cookie need the
`X-Syncr-CSRF: 1` header; bearer calls do not. API tokens cannot create or
delete tokens or change the password, and an admin's token cannot create
users or reset passwords: each of these needs a signed-in session.
