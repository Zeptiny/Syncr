# ADR 0003: rclone and restic as supervised child processes

- Status: accepted
- Date: 2026-11-04

## Context

v1 drove a long-lived `rclone rcd` over HTTP and copied its responses into the
database, which broke when rclone's API changed; credentials were pasted into
connection strings and logs. Embedding rclone or restic as Go libraries would
tie Syncr's release cycle and binary size to theirs and make crashes in
backend code take down the worker.

## Decision

Each job runs as one child process of a pinned, version-checked `rclone` or
`restic` binary, supervised by `internal/worker/proc`:

- **Typed options, no free-form flags.** Runners map the typed `JobSpec`
  (`link.proto`) to an allowlisted set of flags.
- **Remotes are environment, never strings.** Each `RemoteDefinition` becomes
  `RCLONE_CONFIG_<ALIAS>_<OPTION>` variables visible only to that child
  (`internal/worker/rcloneenv`). `RCLONE_CONFIG` points at an empty config
  file in a per-job scratch directory (with `RCLONE_CACHE_DIR` beside it and
  `RCLONE_ASK_PASSWORD=false`), so a host `rclone.conf` is never read; being
  environment variables, these also reach rclone when restic starts it. restic
  reuses the same remotes through `-r rclone:<alias>:<path>`, so every rclone
  backend works as a repository; repository passwords travel in
  `RESTIC_PASSWORD`.
- **Minimal environment.** Children receive only an allowlist of the
  worker's variables (PATH, HOME/USERPROFILE, temp dirs, locale, proxy and CA
  settings, and the variables Windows needs) plus the job's variables. The
  worker's own token and `SYNCR_*` settings never reach them.
- **Progress from JSON.** `rclone --use-json-log --stats 1s` and `restic
  --json` emit JSON lines; the supervisor streams stdout/stderr line by line
  (CRLF-aware, lines up to 1 MiB) to callbacks on the runner's goroutine, and
  runners convert them to cumulative `runner.Progress` snapshots.
- **Cancellation.** On Unix the child runs in its own process group and gets
  SIGINT (both tools clean up on it), then SIGKILL after a grace period. On
  Windows the child is killed. Pipes held open by orphaned grandchildren are
  closed after the same grace period, so a job can never hang the worker.
- **No orphans.** A child dies with the worker even when the worker is killed
  (SIGKILL, OOM, crash), so it cannot keep writing to a remote or repository
  or race with a restarted worker that runs the job again. On Linux it gets
  SIGKILL through the parent death signal (`Pdeathsig`); because the kernel
  ties that signal to the forking thread, not the process, the child is
  started and waited for on a goroutine that keeps its OS thread locked. On
  Windows every child is added, just after it starts, to the worker's Job
  Object (one per supervisor) with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`,
  whose handle the worker never closes; processes the child starts (restic's
  rclone) are in the job too. os/exec cannot start a child suspended, so a
  child is outside the job for a moment after it starts. Other Unix systems
  have neither mechanism.
- **Typed outcomes.** The supervisor returns the exit code with
  `*StartError`, `*ExitError` or `*CanceledError`; runners map them to job
  error codes.

## Consequences

- Upgrading rclone or restic is a pin change plus integration tests
  (`make test-integration`), not a code change.
- A crashing tool fails one job, not the worker.
- Runners depend on the tools' JSON formats; integration tests against the
  pinned versions guard them.
- Windows has no graceful stop for console children yet; a Ctrl+Break based
  stop can be added behind the same API.
- On Linux the parent death signal reaches only the direct child: a process
  it starts is not killed with the worker (restic's rclone exits once restic
  closes its pipes).
