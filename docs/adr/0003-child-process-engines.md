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
  `RCLONE_CONFIG_<ALIAS>_<OPTION>` variables visible only to that child;
  rclone runs with `--config ""` so no config file is read or written. restic
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
  stop or Job Objects (to also reap grandchildren) can be added behind the
  same API.
