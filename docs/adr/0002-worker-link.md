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
