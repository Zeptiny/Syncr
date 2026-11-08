# ADR 0010: Notification contacts and the durable outbox

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-08

## Context

Milestone 4 tells people when their jobs and scheduled runs succeed, fail,
are interrupted or are missed, through channels such as Discord, email and
generic webhooks. Until now the jobs service called a `Notifier` port
synchronously after storing a failed job. That has four problems:

- **At most once.** A crash between storing the job and the call loses the
  notification.
- **It blocks.** The call usually runs on the worker link's ingest goroutine;
  a slow SMTP server would stall the link.
- **No retries.** A target that is down for a minute drops the message.
- **Failures only.** Succeeded jobs and missed scheduled runs never notify.

Further constraints:

- Use cases cannot import each other (depguard): the jobs and schedules
  services cannot call a notifications service inside their transactions,
  and repository ports never span calls with a transaction.
- Targets are user configuration with credentials (webhook URLs carry
  tokens, SMTP passwords), like remotes (ADR 0007).
- One controller today, but the claims must stay safe with two.

## Decision

### Channels are descriptors in a registry

`internal/notify` defines `Channel` (ID, label, description, form fields,
`Validate`, `Summary`, `Send`) and an immutable `Registry`, mirroring the
backend registry. Fields reuse `internal/descriptor`, so the web UI renders
contact forms from `ChannelService.ListChannels` exactly as it renders
remote forms. `Send` returns nil or a `*notify.SendError` (permanent or
temporary, an optional `RetryAfter`, a sanitized `Reason`). The contract
suite `notifytest` checks every channel, built-in or not: validation names
fields and never quotes values, summaries and send errors contain no
secrets, and a cancelled send is a temporary failure. A channel type that
passes the suite needs no other change: an API test registers a fake one
and uses it for a contact, a test send and a planned delivery.

### Contacts follow the remotes pattern

A contact (`internal/app/contacts`) is an instance of a channel type, owned
by one user. Admins included, nobody else can see, use, test or link it
(`ErrNotFound`). The channel validates the merged configuration on create
and update. Secrets are write-only: one JSON object encrypted with the
keyring, bound to the contact by the associated data
`contact-secrets:<id>`, with the key names kept in clear for the UI. An
update merges secrets (a non-empty value replaces, an empty one keeps, and
`clear_secrets` removes optional ones) and replaces options as a whole.
Names are unique per owner, case-insensitively. Delete is a hard delete
that cascades to the schedule links and the delivery log.

`SendTest` is synchronous and bypasses the outbox, so the form gets an
answer at once. It is bounded by `notify.send_timeout` and limited per user
(a burst of 3, then one every 10 s); only valid requests take a token. A
channel failure is a result (`ok: false` with the sanitized reason and
status), not an RPC error.

### Two-stage outbox: plan exactly once, deliver at least once

The source of truth is the store, not a call. Terminal jobs and missed or
failed runs carry `notified_at`, which is NULL until they are planned (the
migration marks all earlier history as notified).

1. **Planning.** The planner reads the outcomes with `notified_at IS NULL`,
   with their subscribers, and builds one delivery per subscriber whose
   events contain the outcome's event. `CommitJobOutcome` and
   `CommitRunOutcome` are one transaction each. They lock the source row,
   insert the deliveries with `ON CONFLICT (contact_id, dedupe_key) DO
   NOTHING`, and set `notified_at`. The dedupe key is `job:<id>:<event>` or
   `run:<id>:<event>`. A crash anywhere just re-plans, concurrent planners
   serialize on the row lock, and either way the unique index stops
   duplicates. A delivery whose contact was deleted meanwhile is skipped:
   the insert checks the contact under `FOR KEY SHARE`, so a concurrent
   delete either waits or has already removed it. No job or schedule code
   takes part in the transaction.
2. **Delivery.** Deliverers claim due deliveries with a lease in one
   statement. The due rows are selected `FOR UPDATE SKIP LOCKED` in a
   materialized CTE and set to `sending`, with `attempts + 1` and
   `lease_until = now + lease` (2 min). A pending delivery is due when its
   `next_attempt_at` has passed, and a sending one when its lease has
   expired (a deliverer died). Claims are disjoint across goroutines,
   pools and controllers. The deliverer resolves the contact, sends with
   the message ID set to the delivery ID, and records the outcome with a
   compare-and-swap on `status = 'sending' AND attempts = <claimed>`, so a
   deliverer whose lease was reclaimed cannot overwrite the newer attempt.
   Receivers deduplicate on the delivery ID (the `webhook-id` header, the
   email `Message-ID`), because a lease that expires mid-send can resend.

### Subscribers

- A **scheduled job** notifies the enabled contacts linked to its schedule
  whose link events contain the event (`schedule_contacts.events`). It
  never notifies through `job_events`.
- A **manual job** notifies the owner's enabled contacts whose `job_events`
  contain the event. The set is empty by default (opt-in).
- A **missed or failed run** notifies the schedule's linked contacts. A run
  that failed because its owner is disabled notifies nobody.
- **Cancelled jobs never notify.** They are marked without deliveries.
- A disabled contact's deliveries that were already planned are still
  sent. A deleted contact's deliveries are deleted with it.

### Retries

- **Permanent errors** make a delivery dead at once: a permanent
  `SendError`, credentials that cannot be decrypted, an unknown channel
  type, or a stored payload that cannot be read.
- **Temporary errors** retry after `min(1h, 30s × 2^(attempt−1))`,
  jittered uniformly into `[d/2, d]`, or after the target's `RetryAfter`
  (capped at an hour).
- **The attempt limit.** When `max_attempts` (8, about an hour) is reached,
  the delivery is dead. An attempt whose lease expired counts. If the
  reclaimed claim exceeds the limit, the delivery is dead without another
  send.
- **Shutdown.** An attempt interrupted by shutdown is recorded, with a
  context that is not cancelled, as pending again at once.
- **Last error.** Only a `SendError`'s sanitized reason (with the status)
  reaches `last_error` and the logs. Any other error could quote the
  configuration, such as a `*url.Error` with a token, and is replaced by a
  generic text.
- **Manual retry.** `RetryDelivery` queues a dead delivery again with its
  attempts reset.
- **Retention.** Delivered and dead deliveries are purged after
  `notify.retention` (30 days).

### Wake-up path

`jobs.Notifier` becomes `JobFinished(Job)`. It must not block, and it only
wakes background loops. The composition root fans it out to
`notifications.Service.Wake` and the scheduler; it also wakes the outbox
when the scheduler fires a schedule or ends a run missed or failed (a
decorator of the scheduler's `Schedules` port in `cmd/syncr-controller`). `Run` makes a pass at once,
on every wake, and every 30 s as a backstop. A pass plans, then delivers in
rounds of `notify.concurrency` (4) claims, and purges once an hour. A lost
wake only delays a notification until the next tick.

## Consequences

- Notifications survive crashes and restarts. The cost is an outbox table
  and a planner pass per wake. The planner is cheap: it reads a partial
  index (`jobs_notify_idx`, `schedule_runs_notify_idx`) that holds only the
  unplanned rows.
- Delivery is at least once, not exactly once. Receivers must deduplicate
  on the delivery ID, and the Standard Webhooks `webhook-id` header makes
  that the norm.
- A delivery's payload (`notify.Message` JSON, no secrets) is frozen when
  it is planned. A schedule renamed later still shows the old name in
  pending retries, which matches what happened.
- A target answers within about 30 s (half the first backoff, plus the tick)
  after a temporary failure at the earliest, and `RetryAfter` values below
  the tick wait for the next pass.
- The delivery log lists the caller's deliveries by ID without an owner
  index. That is fine at 30 days of retention; a `(owner_id, id)` index
  can follow if logs grow.
- The memory store implements the same contract (one mutex stands in for
  the row locks and SKIP LOCKED), and the shared `storetest` suite checks
  both stores. The Postgres tests also race two pools.

## Alternatives considered

- **A synchronous notifier with retries in a goroutine.** Rejected: the
  notification is still lost on a crash, and retries would compete with
  the link for goroutines and memory.
- **Planning inside the jobs and schedules transactions.** Rejected: it
  couples three use cases through one transaction, which the ports and the
  layering forbid, and makes job status changes depend on contact lookups.
- **`LISTEN/NOTIFY` or an external queue.** Rejected: neither is durable on
  its own, and either one adds a moving part to a single-binary
  deployment. The poll and wake design needs nothing beyond the database.
- **One delivery row per outcome, fanned out at send time.** Rejected:
  retries, leases and dead letters are per contact. One row per contact
  keeps every state transition a single-row compare-and-swap.
