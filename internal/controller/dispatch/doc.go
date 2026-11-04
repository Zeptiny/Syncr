// Package dispatch is the controller side of the worker link
// (docs/adr/0002-worker-link.md, proto/syncr/v1/link.proto): it turns
// worker reports into job state changes, derives each worker's commands,
// reconciles after worker restarts, holds the long polls of active workers
// and drives passive workers. It is transport-agnostic: the active transport
// (a ControllerLinkService handler) authenticates the worker and calls
// Report and Poll; the passive transport supplies a PassiveClient per worker
// and runs its PassiveLoop.
//
// Job state lives in the job service and worker link state in the worker
// service, both behind small ports (ports.go). Nothing about commands is
// stored separately: commands are derived from job state on every request.
// The only in-memory state is a per-worker session (cancel acks of the
// current epoch, jobs to cancel from the latest snapshot), which the
// worker's next requests rebuild after a controller restart; a lost session
// at worst re-sends a CancelJob, which workers treat as a no-op.
//
// # Request handling
//
// Every call names the worker (already authenticated by the transport) and
// the worker's epoch. In order:
//
//  1. Epoch: the current epoch is accepted; a replaced epoch (one of the
//     last 16) is rejected with ErrStaleEpoch (FAILED_PRECONDITION: it can
//     only be a delayed request of a dead worker process); any other epoch
//     is adopted by compare-and-swap, the ingest cursor restarts at 0, and
//     the epoch rules below run. Then the contact is recorded (last seen).
//  2. Acks (Report, Poll, passive Command responses).
//  3. Events (Report, passive Pull responses), in seq order, skipping every
//     seq at or below the stored cursor; seqs may have gaps. Each event's
//     effect is idempotent and is stored before the cursor advances past
//     it, so a crash in between only replays the event. In particular the
//     Hello is stored durably before it is acknowledged.
//  4. The state snapshot, if any.
//
// Calls of one worker are serialized within this process; the stores'
// conditional updates (compare-and-swap epoch, monotonic cursor and last
// seen, version-checked job updates) keep concurrent processing correct
// beyond that.
//
// # Events
//
//	Event        Job (this worker)                       Effect
//	Hello        -                                       worker info replaced (if epoch still current)
//	JobStarted   queued                                  dispatched by this epoch, then running
//	JobStarted   dispatched by this epoch                running (started at = event time)
//	JobStarted   anything else                           ignored (duplicate or late)
//	Progress     running in this epoch                   published live; stored if SampleInterval passed
//	Progress     anything else                           ignored
//	JobResult    queued, or dispatched/running in epoch  terminal (implicit dispatch for queued):
//	               SUCCEEDED                               succeeded, with warnings and output
//	               FAILED                                  failed (notifier called)
//	               CANCELLED, cancel requested             cancelled
//	               CANCELLED, no cancel requested          interrupted, worker_shutdown (notifier called)
//	               unknown status                          failed, internal
//	JobResult    terminal                                ignored
//	any job      unknown, malformed or another worker's  skipped and acknowledged (logged)
//	unknown type -                                       skipped and acknowledged (newer worker)
//
// # Acks
//
//	Ack            Effect
//	start:<job>    queued -> dispatched by the acking epoch; otherwise nothing
//	cancel:<job>   that CancelJob is not sent again in this epoch
//	anything else  ignored
//
// # Commands
//
// Derived on every Commands, WaitCommands and passive round, oldest job
// first, at most Config.MaxCommands:
//
//	Job state (this worker)                                   Command
//	queued                                                    StartJob{job_id, spec}, id "start:<job>"
//	dispatched/running in the current epoch, cancel requested,
//	  CancelJob not acked in this epoch                        CancelJob, id "cancel:<job>"
//	listed by the latest snapshot but finished, unknown or
//	  another worker's ("orphan"), not acked in this epoch     CancelJob, id "cancel:<job>"
//
// A queued job never has a cancel request (canceling a queued job
// finishes it at once, and a re-queue of a job with a cancel request
// cancels it instead), so no StartJob is ever sent after a cancel. A queued
// job whose stored spec cannot be decrypted is failed (internal) instead of
// sent. Command IDs are derived from the job, so redeliveries keep their
// ID and workers deduplicate by job ID anyway.
//
// # Reconcile
//
// Epoch rules, run when a new epoch is adopted and again whenever the
// worker's unfinished jobs are loaded (so a half-finished reconcile
// completes on the next call):
//
//	Job (this worker)                                  Becomes
//	running, accepted by another epoch                 interrupted, worker_restarted (notifier called)
//	dispatched by another epoch, no cancel requested   queued (requeues+1; StartJob derived for the new epoch)
//	dispatched by another epoch, cancel requested      cancelled
//	queued                                             unchanged (StartJob derived as usual)
//
// A dispatched job is re-queued because it never started as far as the
// controller knows. If the dead process had started it but its JobStarted
// was lost with the outbox, the job runs again; transfers and backups are
// safe to repeat.
//
// Snapshot rules. The snapshot (WorkerState) is consistent with the events
// of its envelope: a job missing from it has its result in the envelope or
// already acknowledged. Its position snapSeq is WorkerState.last_seq or, if
// that is 0, the envelope's last event seq (0 if it has none: unknown).
// cursor is the ingest cursor after the envelope's events.
//
//	Condition                         Job (this worker)                    Effect
//	snapSeq > 0 and snapSeq < cursor  -                                    stale snapshot: ignored
//	listed                            queued                               dispatched by this epoch (implicit ack)
//	listed                            finished, unknown, another worker's  orphan: CancelJob until acked or no longer listed
//	listed                            dispatched/running in this epoch     consistent, nothing
//	not listed, snapSeq == cursor     running in this epoch                interrupted, worker_lost (notifier called)
//	not listed                        dispatched in this epoch             nothing (the snapshot may predate the acceptance)
//	not listed, snapSeq unknown       running in this epoch                nothing (cannot tell a stale snapshot)
//
// # Timeouts
//
// Run fails jobs that stay queued longer than Config.DispatchTimeout
// (dispatch_timeout); the message says whether the worker never connected,
// is offline (and since when) or is online but did not accept the job. The
// clock starts again when a job is re-queued.
//
// # Cancel, by state
//
//	Job state            Cancel (package jobs)              Then
//	queued               cancelled at once                  if the worker took it concurrently, the snapshot
//	                                                        lists it and an orphan CancelJob stops it
//	dispatched, running  cancel requested, worker woken     CancelJob; the worker's CANCELLED result ->
//	                                                        cancelled; if it finishes first, its outcome stands;
//	                                                        if it restarts, dispatched -> cancelled and
//	                                                        running -> interrupted
//	finished             nothing                            -
//
// # Long polls and passive workers
//
// WaitCommands returns as soon as commands exist, or empty after the wait
// (capped at Config.MaxWait). The Hub wakes it when the job service queues,
// re-queues or asks to cancel a job of the worker, and when a snapshot
// reveals a new orphan; there is no polling. PassiveLoop documents the
// passive cadence (about 1s while busy, 15s while idle, at once while the
// worker has more events).
//
// Allowed dependencies: the standard library, protobuf, the generated link
// code, internal/link, internal/domain and the internal/app services it
// drives (through the ports in ports.go). No adapters.
package dispatch
