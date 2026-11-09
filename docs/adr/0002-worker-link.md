# ADR 0002: Worker link with active and passive modes over one core

- Status: accepted (wire contract refined in Milestone 2; unreleased until v2.0.0)
- Date: 2026-11-04

## Context

Workers run on servers with open ports and on desktops behind NAT that sleep
and roam. Either side may restart at any time. Job state must never be lost
or applied twice, and the protocol must traverse ordinary HTTP proxies and be
debuggable with curl.

## Decision

One set of messages (`proto/syncr/v1/link.proto`), two transports, chosen
per worker:

- **Active** (worker initiates): `ControllerLinkService.Report(epoch, events,
  acks, state) → ack_seq` about once a second while busy and as a ~15 s
  heartbeat; `ControllerLinkService.Poll(epoch, wait ≈ 25 s) → commands` as a
  long poll.
- **Passive** (controller initiates; worker serves TLS with a certificate
  whose fingerprint is pinned at enrollment): `WorkerLinkService.Command(cmds)
  → acks`; `WorkerLinkService.Pull(epoch, after_seq) → (epoch, events, state)`.

All calls are unary Connect RPCs over HTTP/1.1. Identity is never taken from
message bodies; secrets are compared in constant time:

- Active: `Authorization: Bearer <worker_id>.<secret>` on every
  ControllerLinkService call.
- Passive: the controller sends `Authorization: Bearer <secret>` and
  authenticates the worker by its TLS certificate, whose SHA-256 fingerprint
  was pinned at registration (no CA).
- Enrollment: `EnrollmentService.Enroll` is the only unauthenticated link
  call. An admin creates a short-lived, single-use enrollment token (only its
  hash is stored); `syncr-worker setup --token` exchanges it once for a
  worker ID and a random secret. Passive workers can instead print a pairing
  secret, URL and fingerprint at setup, which the admin registers.

The controller stores worker secrets encrypted (AES-256-GCM with key IDs,
bound to their record), not hashed, because it must send passive workers
theirs.

**Outbox, seq and epoch.** Every progress sample, job start and job result is
an `Event` with a `seq` that increases strictly within an `epoch`, a random ID
chosen at worker process start. Events stay in the worker's in-memory outbox
until the controller acknowledges the highest seq it has durably stored.
Under backlog, progress events of the same job are merged into the latest
one; `JobResult` is never dropped or merged. The epoch travels in the
envelope, not in each event.

**Idempotency.** The controller ingests by `(worker, epoch, seq)` and ignores
duplicates; seqs may have gaps (coalesced progress). Each event's effect is
idempotent and stored before the ingest cursor advances past it. Commands are
not stored: they are derived from job state on every request (queued job:
`StartJob`; cancel requested: `CancelJob`), with IDs derived from the job
(`start:<job>`, `cancel:<job>`) so redeliveries keep their ID; workers also
deduplicate by job UUID. An ack only means "accepted"; outcomes (including
rejection of an invalid spec) arrive as events. A request carrying an epoch
the controller has already replaced is rejected, so a delayed request of a
dead worker process cannot undo a reconcile.

**Reconcile** runs when the epoch changes and on every `WorkerState`
snapshot (carried by every Report/Pull and consistent with its events; its
`last_seq` lets the controller ignore stale snapshots):

- on a new epoch, jobs the old process was running become `interrupted`, and
  jobs it had accepted but not started go back to `queued` (or `cancelled`
  if a cancel was requested);
- a running job missing from a current snapshot becomes `interrupted`;
- a queued job listed by the snapshot counts as accepted;
- jobs the worker still holds that the controller considers finished or does
  not know are cancelled.

The same holds without a restart when a worker shuts down gracefully: a
CANCELLED result nobody asked for (`WORKER_SHUTDOWN`) interrupts a job that
started, but a job that never started (no `JobStarted` before its result)
goes back to `queued`, without a notification, for the worker's next
process.

The full rule tables live in the package documentation of
`internal/controller/dispatch`. A job that stays queued longer than the
dispatch timeout (worker offline) fails with `DISPATCH_TIMEOUT`.

**Secrets.** `JobSpec` is built at dispatch time and may carry remote
credentials and repository passwords. It is sent once over the authenticated
link, kept only in worker memory and never persisted or logged; the
controller stores its own secret-free job definition.

## Consequences

- The same ingest, reconcile and command derivation code
  (`internal/controller/dispatch`) serves both modes; only the transport
  differs. `internal/link` holds helpers both sides share (job kinds, error
  code names, authorization headers).
- A worker restart loses buffered progress samples (results of jobs that were
  still running are reconstructed as `interrupted`), which is acceptable;
  controller restarts lose nothing because workers resend unacked events.
- Long polls must end promptly on controller shutdown; Milestone 2 ties them
  to the server's lifetime.

## Worker side (Milestone 2)

The worker's link core is three packages: `internal/worker/outbox` (events,
seq, ack, coalescing), `internal/worker/agent` (commands, job execution) and
`internal/worker/linkcore` (envelopes for both transports, no networking).
What the controller can rely on:

- **Seqs have gaps.** Under backlog (`CoalesceAt` unacked events) a new
  Progress event of a job replaces that job's unacked Progress events, which
  are removed; the new one gets a fresh seq. Seqs still strictly increase,
  Hello/JobStarted/JobResult are never dropped, a job's Progress events lie
  between its JobStarted and its JobResult, and an event's content never
  changes once it has a seq. Ingest must not expect contiguous seqs.
- **Backpressure, not loss.** At `HighWater` unacked events the worker stops
  starting queued jobs until the controller acks; running jobs still report
  progress (coalesced) and results.
- **State is consistent with the events beside it.** The `WorkerState` in a
  Report or Pull lists the jobs the worker holds as of the last event in that
  message, including jobs whose JobResult exists but did not fit in the
  batch. So "dispatched job missing from the state and without a result"
  really means the worker lost it (new epoch), and a job whose result is in
  the batch is never listed.
- **Acks.** An event ack beyond the worker's last seq is ignored (never
  trusted). A Pull whose epoch is not the worker's ignores `after_seq` and
  starts at the oldest unacked event.
- **Cancellation.** A CANCELLED JobResult carries `JobError` with code
  CANCELLED ("job cancelled by the controller") or WORKER_SHUTDOWN ("...
  because the worker is shutting down"). The result of a job that never
  started (it waited in the local queue) has no `started_at` and no
  JobStarted before it. A StartJob received while the worker shuts down
  is refused: it is not acknowledged and leaves no event, so the job stays
  queued on the controller for the worker's next process.
- **Sizes.** A Report or Pull carries at most about 4 MiB of events
  (`PullRequest.max_bytes`, capped at 8 MiB) but always at least one event.
  A single event stays far below the 16 MiB read limits: a result's output
  is capped at 4 MiB (a long restic snapshot list keeps the newest
  snapshots and says so in a warning) and its error message at 4 KiB.
- **State fields.** `WorkerState.last_seq` is the seq of the envelope's last
  event, or the last assigned seq when the envelope has none;
  `outbox_backlog` and `outbox_saturated` describe the whole outbox.

## Transports (Milestone 2)

- Controller, active workers: `internal/controller/dispatch/active` serves
  ControllerLinkService and EnrollmentService. Authentication runs as a
  Connect request gate (before the body is read); Enroll is rate limited per
  client IP. Long polls end when the server's lifetime context ends, and the
  handler extends the connection's read and write deadlines for a Poll, so
  server-wide `ReadTimeout`/`WriteTimeout` values cannot cut a legitimate
  long poll.
- Controller, passive workers: `internal/controller/dispatch/passive` has the
  pinned HTTPS client (SHA-256 of the leaf certificate's DER, no CA, no host
  name check) and the supervisor that keeps one `PassiveLoop` per passive
  worker.
- Worker: `internal/worker/transport/active` runs the Report and Poll loops
  of the linkcore contract; `internal/worker/transport/passive` serves
  WorkerLinkService over TLS.
- How the binaries wire all of this, and the order in which they shut down,
  is in [ADR 0005](0005-runtime-wiring.md).
