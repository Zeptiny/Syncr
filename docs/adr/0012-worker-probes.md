# ADR 0012: Worker probes

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-09

## Context

Milestone 5 needs answers that only a worker can give and that a person is
waiting for: the folders of a worker's disk (to choose a backup source or a
restore target), the folders of a remote, whether a remote's credentials
work, a restic repository's snapshots and the contents of one snapshot
folder, and whether a repository can be created or opened before it is
saved.

The only way to make a worker do something was a job. Jobs are the wrong
tool for this:

- They take a job slot (`max_concurrent_jobs`), so browsing would wait
  behind a running backup.
- They travel through the outbox (reports about once a second) and the
  dispatch timeout (15 minutes), and they are stored: every click in a
  folder picker would add a row to the jobs table, the dashboard and the
  notification planner.
- Their results are capped by event sizes and arrive asynchronously, while
  the API caller wants one synchronous answer or an error.

Further constraints:

- The link has two modes (ADR 0002). In passive mode the controller calls
  the worker; in active mode only the worker opens connections.
- Workers older than Milestone 5 acknowledge and ignore commands they do
  not know, so a new command sent to them would vanish without an error.
- Specs carry credentials (remote secrets, repository passwords), like job
  specs.
- The API layer cannot import controller packages, and use cases cannot
  import each other (depguard).

## Decision

### Probes are request/response over the link

A probe is a short operation (`ProbeSpec.op`: `list_local`, `list_remote`,
`about`, `restic_snapshots`, `restic_ls`, `restic_init`, `restic_config`)
that a worker runs outside its job slots and answers with one
`ProbeResult`. Nothing about a probe is stored on either side.

- **Passive workers** get a new unary RPC, `WorkerLinkService.Probe`. The
  controller calls it with the API caller's context plus a deadline, so
  canceling the HTTP request cancels the probe.
- **Active workers** receive `Command.run_probe` through their long poll
  and answer with a new unary RPC, `ControllerLinkService.CompleteProbe`.
  If the caller goes away once the `run_probe` may have reached the worker,
  the worker is sent `Command.cancel_probe`.

Restic probes read with `--no-lock` and take no concurrency keys
(ADR 0013), so browsing never waits for, or blocks, a backup.

### A broker in the dispatcher

`dispatch.ProbeBroker` is the only thing that knows both modes. Its `Probe`
method is what the rest of the controller calls.

For active workers it keeps a table of waiting calls in memory. The
dispatcher asks it for commands on every derivation. Probe and job
commands share the dispatcher's cap on commands per response: the
`CancelJob`s go first, then the probe commands get up to half of the
remaining room (at most 16 `run_probe` at once) and the `StartJob`s the
rest, each side taking what the other leaves. So a long job queue cannot
starve a person waiting for a folder listing, and probes never hold back a
job's cancel. Command IDs are
derived from the probe (`probe:<id>`, `probe-cancel:<id>`), as job commands
are derived from the job:

| Event | Effect |
|---|---|
| the worker acks `probe:<id>` | the probe is not sent again |
| no ack yet | `run_probe` is derived again on the next poll (workers deduplicate by probe ID) |
| the caller gives up before a `run_probe` was derived for the worker's current process | the call is dropped; nothing is sent |
| the caller gives up after that, acked or not | `cancel_probe` is sent until it is acked |
| the worker's epoch changes | accepted probes fail ("the worker restarted"); the others go to the new process |
| the Hello of the worker's current process lacks `probe` or the operation | its waiting probes fail at once as outdated |
| `CompleteProbe` | matched by probe ID **and** worker; unknown, late or another worker's results are accepted and ignored |

`CompleteProbe` authenticates the worker and adopts its epoch like every
other link call, so a result from a replaced worker process is refused.

A `run_probe` counts as sent once a derivation includes it, even if the
command budget leaves it out of that response: the worker may already
have it (a Poll response the controller wrote but the worker had not
acked yet), and a probe it started would otherwise run to the end, such
as a `restic init` creating a repository nobody stores, or a slow listing
holding one of the worker's probe slots. A worker that never saw the probe
ignores the cancel. Workers remember a cancelled probe ID as long as a
started one (three times the cap), and apply the cancels of a Poll response
before its `run_probe`s, so a cancel that arrives first, in the same
response or before a redelivery, keeps the probe from starting.

For passive workers the `PassiveLoop` registers its client with the broker
while it runs, and `Probe` calls the worker directly. The passive client
uses a second HTTP transport for probes: the normal one gives up after
30 s, a probe may take up to the longer probe setting plus the grace, and
its context carries the deadline. A caller that gives up ends the call,
and the worker's handler stops the probe.

The broker lives in `dispatch` and not in the probe package because it
shares the dispatcher's sessions, command derivation and `Hub`. It is
handed to the dispatcher (`Deps.Probes`) and, as a `Transport`, to the
probe runner, which breaks what would otherwise be a wiring cycle.

### Version gating

Workers report link features (`Hello.features`: `probe`, `restic.v2`,
`restic.delete`) and the probe operations they implement
(`Hello.probe_ops`). The controller refuses a probe up front when the
worker lacks the `probe` feature or the operation, with a message that says
to upgrade the worker. It never sends `run_probe` to a worker without the
feature: the dispatcher decides from the Hello of the worker's current
process, and a new process's probes wait until its Hello is stored (the
up-front check may still see the previous process's Hello). Storing that
Hello fails the waiting probes it cannot run with the same error, at once
rather than at their timeout. A passive worker that answers
`Unimplemented` anyway (a stale Hello) gets the same error. An old active worker that somehow receives
`run_probe` acks and ignores it; the probe then times out as unavailable.

### Orchestration lives in `internal/controller/probe`

A probe crosses remotes, repositories, workers, jobs and the link, so its
orchestration is controller code, like the scheduler:

- `Runner` applies the caller's rate limit, resolves the remotes for the
  **caller's own user** (the `SpecBuilder` pattern: another user's remote is
  `ErrNotFound`, for admins too), builds the spec with the same
  `jobkind.Locate` that job specs use, picks the worker, sends the probe
  and maps the outcome. It implements `restic.Prober`.
- `Service` is the API-facing part. It returns generated response types,
  because the API layer may not import controller packages.
- `Pick` is a pure function. An explicitly chosen worker is validated
  (missing: not found; offline: unavailable; outdated: failed
  precondition). Otherwise the eligible workers (online, with the feature
  and the operation) are ordered: the worker of the repository's newest
  succeeded job first, because its restic cache is warm, then the fewest
  unfinished jobs, then name, then ID.

Some targets need an explicit worker, because only one worker can answer:
`list_local` always, and anything on a remote of the `local` backend type,
which is a folder of one worker's disk. Such a remote is browsed with
`list_local` on the joined path, since a worker's rclone prober only takes
real remotes; the response paths are made relative to the remote's root
either way.

### Limits

| Limit | Value |
|---|---|
| Probe timeout, listings and usage | 30 s (`probe.timeout`) |
| Probe timeout, restic operations | 60 s (`probe.restic_timeout`) |
| Hard cap on any probe | 90 s, on both sides (`link.MaxProbeTimeout`) |
| Controller's own wait | the timeout plus 5 s of grace (`link.ProbeGrace`) |
| Probes in flight per worker (controller) | 8; more fail at once as unavailable |
| Probes running per worker (worker) | 4; more answer `BUSY` |
| Per user | a burst of 20, then 2 per second |
| Entries per listing | 2000 by default, 5000 at most |
| Snapshots per listing | the newest 5000 |

The cap and the grace live in `internal/link`, which both sides import;
every other limit is derived from them: the settings' upper bound, the
broker's cap (the longer setting), the passive client's wait for a probe's
answer (that plus the grace), the worker's cap, its passive server's
write timeout and how long it keeps probe results (twice the cap) and
probe IDs (three times).

A local listing stops with its probe's context between batches of
directory entries and between stats. A filesystem call blocked in the
kernel (a hung network mount) cannot be interrupted: the probe still
answers at its deadline, and the listing keeps its probe slot until the
call returns, so stuck listings never exceed the worker's slots.

### Errors

Probe failures leave the controller as domain errors. A new
`domain.ErrUnavailable` maps to Connect `Unavailable` and, like
`ErrFailedPrecondition`, carries a message written for people.

| Condition | Error |
|---|---|
| foreign or missing remote, repository or worker | not found |
| the path is outside `local_roots`, or does not exist | validation error on `path` |
| the stored password no longer opens the repository; no repository at the location | failed precondition |
| timeout, busy, no answer, worker restarted or offline | unavailable |
| outdated worker, no usable worker, any other tool failure | failed precondition |

A failure the worker reported also carries its code and message
(`restic.ProbeError`), which the repository use cases turn into form field
errors. `TestRemote` is the exception: what the worker reports is the
answer (`ok=false` with the reason), not an RPC error, like
`SendTestNotification`.

### Secrets

Specs are built per request and never stored or logged, on either side.
Passwords and remote secrets reach only the child process's environment.
Workers scrub their error messages, and the controller scrubs every message
again with the spec's secrets (`redact.Scrub`) before it is logged or
returned. Results hold file names, sizes and usage only.

## Consequences

- Browsing and testing work while a worker's job slots are full, and add
  nothing to the jobs table.
- Probes are lost on a controller restart: waiting callers get an error and
  retry. That is acceptable for requests a person is watching.
- Any signed-in user can list any worker's `local_roots`. Workers are
  shared and users can already run jobs over those paths, so this adds no
  privilege, but it does make the folder names visible.
- A probe on an active worker costs up to one poll round trip more than on
  a passive one.
- Two controllers would each keep their own table: a result delivered to
  the controller that did not send the probe is ignored and the caller
  times out. Probes therefore need sticky routing of a worker's link to one
  controller, which a single controller gives for free.

## Alternatives considered

- **Hidden ephemeral jobs.** Rejected for the reasons in the context: job
  slots, outbox latency, dispatch timeouts, storage and size caps.
- **A streaming RPC per worker.** It would make cancellation and delivery
  simpler, but the link is deliberately unary (ADR 0002): it survives
  proxies and needs no connection state.
- **Running rclone and restic on the controller.** The controller would
  need the tools, network access to every backend and, for local remotes
  and restore targets, the workers' disks.
