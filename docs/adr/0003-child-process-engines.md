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
- **A job is over only when its whole process tree is gone** (Milestone 6).
  A tool's own children (restic's `rclone serve restic`, or anything a
  backend starts) must not outlive a cancelled, timed-out or finished job:
  they would keep a repository lock or a file open while the next job on it
  starts.
  - **Linux:** a cancel still sends SIGINT to the direct child only, so
    restic can release its repository lock. Once the child has exited (by
    itself, on the SIGINT, or killed after the grace period), the rest of its
    process group gets SIGKILL, and only then is the child reaped. The
    worker waits with `waitid(P_PID, pid, WEXITED|WNOWAIT)`, which returns
    once the child exited but leaves it an unreaped zombie. A zombie keeps
    its PID, so when `kill(-pid, SIGKILL)` runs the group ID cannot have been
    recycled for an unrelated process: the only members are the child and
    its descendants (a process can only join a group of its own session, and
    a new group takes its leader's PID). The child is reaped by `cmd.Wait`
    on the same goroutine, after the signal, and nothing else in the worker
    reaps children. If `waitid` fails, no group signal is sent at all.
  - **Windows:** each run also gets a Job Object of its own, nested in the
    worker-wide one (Windows 8 and later), with kill-on-close.
    `TerminateJobObject` runs on cancel and again after the child exited,
    then the handle is closed. If the job cannot be created or assigned (the
    worker itself may run in a job that forbids nesting), the run continues
    as before, with a warning. A grandchild started between the child's start
    and its assignment escapes the job (os/exec cannot start a child
    suspended).
  - **Other Unix systems:** only the direct child is stopped.
  - restic runs with the scratch root as its working directory and the
    per-job directory as its TMP, so a lingering process cannot hold the
    job's directory open.
- **Time limits** (ADR 0014). A job with a time limit runs under a context
  that ends when the limit is reached, with its own cause; the supervisor
  then stops the tree exactly as for a cancel, and the job is reported
  `FAILED` with `TIME_LIMIT`.
- **One worker per data directory, a clean start.** `syncr-worker run`
  takes an exclusive OS lock on `<data_dir>/worker.lock` before it builds
  anything (a second worker on the same directory exits and says why), then
  removes everything under `scratch/rclone` and `scratch/restic`: the job
  directories of a process that died. The OS releases the lock when the
  process dies, so a crash never leaves a stale lock.
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
  closes its pipes). While the worker runs, the group kill above stops such
  processes at the end of every job.
- The tree kill is covered by tests whose helper starts a grandchild that
  ignores SIGINT: it is gone after a normal exit, a cancel, a time limit and
  a kill after the grace period. The Windows variant is proven only by the
  Windows CI.
