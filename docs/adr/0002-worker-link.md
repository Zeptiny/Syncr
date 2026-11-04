# ADR 0002: Worker link with active and passive modes over one core

- Status: accepted (wire contract is a Milestone 1 draft, refined in Milestone 2)
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

All calls are unary Connect RPCs over HTTP/1.1. Workers authenticate with a
per-worker bearer token in the `Authorization` header, compared in constant
time; identity is never taken from message bodies.

**Outbox, seq and epoch.** Every progress sample, job start and job result is
an `Event` with a `seq` that increases strictly within an `epoch`, a random ID
chosen at worker process start. Events stay in the worker's in-memory outbox
until the controller acknowledges the highest seq it has durably stored.
Under backlog, progress events of the same job are merged into the latest
one; `JobResult` is never dropped or merged. The epoch travels in the
envelope, not in each event.

**Idempotency.** The controller ingests by `(worker, epoch, seq)` and ignores
duplicates. Commands carry an ID for acks; `StartJob` and `CancelJob` are
idempotent by job UUID on the worker. An ack only means "accepted"; outcomes
(including rejection of an invalid spec) arrive as events.

**Reconcile** runs when the epoch changes or a worker reconnects, using the
`WorkerState` snapshot carried by every Report/Pull:

- jobs the controller considers dispatched or running but the worker does not
  hold become `interrupted`;
- jobs the worker still holds that the controller considers finished are
  cancelled.

**Secrets.** `JobSpec` is built at dispatch time and may carry remote
credentials and repository passwords. It is sent once over the authenticated
link, kept only in worker memory and never persisted or logged; the
controller stores its own secret-free job definition.

## Consequences

- The same ingest, reconcile and command-queue code serves both modes; only
  the transport differs (`internal/link` holds the shared logic).
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
  UNSPECIFIED and the cause as message ("job cancelled by the controller" or
  "... because the worker is shutting down"). A StartJob received while the
  worker shuts down is acked and reported CANCELLED.
