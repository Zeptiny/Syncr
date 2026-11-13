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
//	Hello        -                                       worker info replaced, as the info of epoch (if
//	                                                       epoch still current); wakes a held poll
//	JobStarted   queued                                  dispatched by this epoch, then running
//	JobStarted   dispatched by this epoch                running (started at = event time)
//	JobStarted   anything else                           ignored (duplicate or late)
//	Progress     running in this epoch                   published live; stored if SampleInterval passed
//	Progress     anything else                           ignored
//	JobResult    queued, or dispatched/running in epoch  terminal (implicit dispatch for queued):
//	               SUCCEEDED                               succeeded, with warnings and output
//	               FAILED                                  failed (notifier called)
//	               CANCELLED, cancel requested             cancelled
//	               CANCELLED, no cancel requested,         not terminal: queued again (requeues+1, StartJob
//	                 job never started (see below)           derived again; not notified)
//	               CANCELLED, no cancel requested,         interrupted, worker_shutdown (notifier called)
//	                 job started
//	               unknown status                          failed, internal
//	JobResult    terminal                                ignored
//	any job      unknown, malformed or another worker's  skipped and acknowledged (logged)
//	unknown type -                                       skipped and acknowledged (newer worker)
//
// A worker stops jobs without a cancel request only when it shuts down. A
// job it never started is still dispatched when its result arrives (a
// JobStarted always comes first and is never dropped) and the result has
// no start time: nothing ran, so it goes back to the queue like a job of a
// restarted worker (see the epoch rules) and the worker's next process
// receives it. A shutting-down worker does not acknowledge StartJob, so a
// job sent to it meanwhile stays queued.
//
// # Acks
//
//	Ack            Effect
//	start:<job>    queued -> dispatched by the acking epoch; otherwise nothing
//	cancel:<job>   that CancelJob is not sent again in this epoch
//	probe:<probe>  that RunProbe is not sent again (see Probes)
//	probe-cancel:<probe>  that CancelProbe is not sent again
//	anything else  ignored
//
// # Commands
//
// Derived on every Commands, WaitCommands and passive round:
//
//	Job state (this worker)                                   Command
//	queued, not waiting on a concurrency key, the Hello of
//	  the current epoch stored                                StartJob{job_id, spec}, id "start:<job>"
//	queued, an older unfinished job shares one of its keys    none (see Concurrency keys)
//	queued, no Hello of the current epoch yet                 none (see below)
//	dispatched/running in the current epoch, cancel requested,
//	  CancelJob not acked in this epoch                        CancelJob, id "cancel:<job>"
//	listed by the latest snapshot but finished, unknown or
//	  another worker's ("orphan"), not acked in this epoch     CancelJob, id "cancel:<job>"
//
// At most Config.MaxCommands commands go out at once, chosen in this order:
// the CancelJob commands (jobs oldest first, then orphans), because someone
// asked for the work to stop and they are few; then the probe commands of
// an active worker (see Probes), up to half of the room the cancels leave;
// then the StartJob commands, oldest job first, in the rest. Room the
// StartJobs leave unused goes to the probes, and the other way round, so
// neither starves the other and probes never crowd out a CancelJob. The
// response lists the probe commands first, then the cancels, then the
// starts.
//
// The stored worker info (link features, operating system) belongs to the
// epoch whose Hello reported it (workers.Worker.InfoCurrent). A new
// process's first Poll can overtake the Report that carries its Hello, and
// until that Hello is stored the info is the previous process's: another
// version after an upgrade or a downgrade, maybe another operating system.
// The dispatcher decides nothing from it: StartJob and probe commands wait
// (the job stays queued, nothing fails) until the Hello of the current
// epoch is stored, and storing it wakes the poll that was held back.
// CancelJob commands do not wait. A process whose Hello never arrives keeps
// its jobs queued until the dispatch timeout fails them (see Timeouts) and
// its probes until they time out.
//
// A queued job never has a cancel request (canceling a queued job
// finishes it at once, and a re-queue of a job with a cancel request
// cancels it instead), so no StartJob is ever sent after a cancel. Command
// IDs are derived from the job, so redeliveries keep their ID and workers
// deduplicate by job ID anyway.
//
// # Concurrency keys
//
// A job may hold concurrency keys, set when it was created (restic jobs
// hold "restic-repo:<repository id>", two-way syncs "bisync:<hash>" for
// each of their two locations; docs/adr/0013-concurrency-keys.md).
// Of the unfinished jobs (queued, dispatched, running; any worker) that
// share a key, only the oldest by (created at, ID) may start: a queued job
// with an older unfinished sharer gets no StartJob. The dispatcher asks
// the job service for these blockers once per derivation (Jobs.Blockers,
// one query) and keeps nothing: every controller and every poll derive the
// same answer from the stored jobs. When a job holding keys finishes, the
// job service wakes the workers of the queued jobs sharing them (Hub), so
// the next one starts at once.
//
// # Specs
//
// Jobs store a secret-free definition. The StartJob spec is built from it
// on every derivation (SpecBuilder, normally Specs): the definition is
// decoded and checked, the remotes it uses are resolved for the job's owner
// with their credentials (for a restic job also its repository, with its
// password, and the repository's remote), and the job kind builds the spec. The spec exists
// only in memory and on the authenticated link; it is never stored or
// logged. A job whose spec can never be built is failed instead of sent:
//
//	Cause                                                  Error code
//	a remote is deleted, missing or not the owner's        remote_unavailable
//	the job's restic repository is deleted or not the
//	  owner's                                              repository_unavailable
//	the worker runs another operating system than the
//	  definition needs (jobkind RequiredOS: Windows VSS)   invalid_spec, saying which system
//	the worker lacks a link feature the kind or the
//	  definition requires (an older version; jobkind
//	  RequiredFeatures, DefinitionFeatures)                worker_outdated
//	the job has a time limit and the worker does not
//	  report link.FeatureJobTimeLimit                      worker_outdated, saying so
//	the job names hooks and the worker does not report
//	  link.FeatureJobHooks                                 worker_outdated, saying so
//	the worker does not define a hook the job names
//	  (what its current process reported in its Hello)     invalid_spec, naming the hooks
//	the definition cannot be decoded, checked or built     invalid_spec
//	a remote's credentials cannot be used (decryption)     internal
//	anything else (e.g. the store is down)                 none: the call fails, the next poll retries
//
// The spec carries the job's time limit (JobSpec.max_runtime;
// docs/adr/0014-job-time-limits-retention.md): its definition's
// max_runtime, else SpecsConfig.DefaultMaxRuntime, except for the kinds
// only the system starts (Specs.TimeLimit). The worker enforces it and
// reports FAILED with TIME_LIMIT, which ends the job failed (time_limit).
//
// The spec also carries the hooks the definition names (JobSpec.hooks;
// docs/adr/0017-job-hooks.md): only their names, which the worker resolves
// to the commands its operator defined. The worker runs them around the
// operation and reports each run with the result (JobResult.hook_runs,
// stored as jobs.Job.HookRuns). A hook that fails the job is reported as
// FAILED with HOOK_FAILED; when that happens after the operation
// succeeded, the result still has the operation's output and warnings,
// which are kept whatever the result's status.
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
// (dispatch_timeout), except jobs waiting on a concurrency key: their
// timeout counts from when they became the oldest unfinished job of their
// keys; the message says whether the worker never connected,
// is offline (and since when), is online but its process never reported
// what it can run (no Hello) or is online but did not accept the job. The
// clock starts again when a job is re-queued, and it starts no earlier than
// the Service: after a controller outage queued jobs get the full timeout
// for their workers to reconnect.
//
// Run also ends the jobs a worker can no longer report on (ExpireLost).
// A worker is silent from its last link call, or from the Service's start
// if that is later (a controller outage never counts against a worker):
//
//	Worker silent for            Job (this worker)                     Becomes
//	Config.CancelGrace (5m)      dispatched/running, cancel requested  cancelled (the CancelJob cannot be
//	                               at least CancelGrace ago              delivered)
//	Config.LostAfter (1h)        dispatched/running, cancel requested  cancelled
//	Config.LostAfter (1h)        dispatched/running, no cancel request interrupted, worker_lost (notifier called)
//
// If the worker comes back with the same epoch, nothing special happens:
// its late JobStarted, Progress and JobResult events find a finished job
// and are acknowledged without effect, and its snapshot lists jobs the
// controller considers finished, which are orphans and get a CancelJob. A
// worker that comes back with a new epoch held nothing from before.
//
// # Cancel, by state
//
//	Job state            Cancel (package jobs)              Then
//	queued               cancelled at once                  if the worker took it concurrently, the snapshot
//	                                                        lists it and an orphan CancelJob stops it
//	dispatched, running  cancel requested, worker woken     CancelJob; the worker's CANCELLED result ->
//	                                                        cancelled; if it finishes first, its outcome stands;
//	                                                        if it restarts, dispatched -> cancelled and
//	                                                        running -> interrupted; if it stays out of
//	                                                        contact for CancelGrace, cancelled
//	finished             nothing                            -
//
// # Long polls and passive workers
//
// WaitCommands returns as soon as commands exist, or empty after the wait
// (capped at Config.MaxWait). The Hub wakes it when the job service queues,
// re-queues or asks to cancel a job of the worker, when a snapshot
// reveals a new orphan, and when the Hello that a derivation waited for is
// stored; there is no polling. PassiveLoop documents the
// passive cadence (about 1s while busy, 15s while idle, at once while the
// worker has more events).
//
// # Probes
//
// A probe is a short request a worker answers outside its job slots
// (docs/adr/0012-worker-probes.md). ProbeBroker.Probe runs one and waits
// for its result; nothing about probes is stored. The broker refuses
// workers that are missing, offline, or lack the "probe" feature or the
// operation (workers older than Milestone 5 would ignore the command).
//
// Active workers: the call waits in the broker's table and the worker's
// long poll is woken. Its commands share Config.MaxCommands with the job
// commands (see Commands; at most 16 RunProbe at once) and wait for the
// Hello of the worker's current process:
//
//	Probe call                                     Command
//	waiting, RunProbe not acked                    RunProbe, id "probe:<probe>"
//	given up by its caller once a derivation for
//	  the current process included its RunProbe
//	  (acked or not)                               CancelProbe, id "probe-cancel:<probe>", until acked
//	given up before that                           none
//
// A RunProbe counts as sent as soon as a derivation includes it, also when
// the command budget then leaves it out of the response: the caller may
// give up between the two, and a worker ignores a CancelProbe for a probe
// it never saw. Workers remember a cancelled probe ID, so a CancelProbe
// that overtakes its RunProbe keeps the probe from starting.
//
// The result arrives with CompleteProbe, which authenticates the worker
// and adopts its epoch like Report; it is matched by probe ID and worker,
// and an unknown, late or another worker's result is ignored. When the
// worker's epoch changes, the probes its previous process had acked fail
// (domain.ErrUnavailable) and the others are sent to the new process. When
// the Hello of the worker's current process is stored, the waiting probes
// it cannot run (no "probe" feature, or not the operation: a downgrade)
// fail at once with the same domain.ErrFailedPrecondition as an up-front
// refusal; derivations repeat that check for a probe queued on the
// previous process's Hello meanwhile. A call ends with
// domain.ErrUnavailable after its timeout plus ProbeConfig.Grace, and at
// most ProbeConfig.MaxInFlightPerWorker calls wait per worker.
//
// Passive workers: a running PassiveLoop registers its client with the
// broker, and Probe calls the worker's Probe RPC directly with the
// caller's context and the same deadline; a caller that gives up ends the
// call and with it the probe on the worker. A worker that answers
// Unimplemented (ErrProbeUnsupported) is reported as outdated.
//
// Time limits: the cap on a probe's timeout and the grace come from
// internal/link (MaxProbeTimeout, ProbeGrace), which the workers use too;
// the composition root caps the broker at the longer of the controller's
// probe settings and lets the passive client wait that long plus the
// grace for a Probe's answer (ProbeConfig.Wait).
//
// Allowed dependencies: the standard library, protobuf, the generated link
// code, internal/link, internal/domain, internal/jobkind and the
// internal/app services it drives (through the ports in ports.go). No
// adapters.
package dispatch
