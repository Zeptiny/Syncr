// Package scheduler fires due schedules and launches their runs as jobs
// (Milestone 4). It crosses use cases (schedules, jobs, workers, users and
// the job kinds), so it lives in the controller layer, like dispatch.
//
// # Tick
//
// Run ticks every Config.Tick (and when woken: a schedule was saved, a run
// added, a job finished). One Tick:
//
//  1. fires the due schedules (schedules.Service.FireDue: a
//     compare-and-swap of each schedule's NextRunAt plus its run, in one
//     transaction);
//  2. walks the open runs, oldest first, and for each pending one decides
//     (in this order): past its deadline it is missed; behind an active run
//     (overlap skip or queue) it waits; a disabled owner fails it and pauses
//     the schedule; a definition the job kinds reject fails it and pauses
//     the schedule; then the target: a removed worker fails it and pauses
//     the schedule, so does one that reported another operating system
//     than the definition needs (invalid_definition: Windows VSS on a Linux
//     worker), an offline or never connected one keeps it waiting; a label
//     without a worker that may run it fails it (no_worker_with_label, no
//     pause), and otherwise the least busy online worker with the label
//     that runs the kind, with its link features and operating system, is
//     chosen (fewest unfinished jobs, then name), or the run waits. What
//     the workers reported is their latest Hello, a hint: the dispatcher
//     checks the chosen worker's current process again.
//  3. launches in two idempotent phases: MarkLaunching reserves a job ID on
//     the run (pending -> launching, a compare-and-swap), jobs.Create
//     creates the job with that ID (ErrAlreadyExists counts as success),
//     MarkLaunched closes the run (launching -> launched). A run found
//     launching (a crash or an error after the reservation) is completed
//     with its reserved ID on the next tick, whatever its deadline: the
//     launch was decided in time.
//
// # Pausing during a launch
//
// Pausing a schedule cancels its pending run, but a launching run's job
// may already exist, so the pause only records a cancel request on it
// (schedules.Run.CancelRequested), which forbids launching -> launched. A
// run with a cancel request is aborted (whenever a pass finds it, after a
// restart too): its job, if it was created, is cancelled (jobs.Cancel,
// idempotent), then the run ends cancelled. When MarkLaunched loses its
// compare-and-swap after the job was created, the pass re-reads the run:
// with a cancel request it aborts it; if the run ended without launching
// (another controller aborted it) or is gone with its schedule, it cancels
// the job; a job runs only for a launched run.
//
// Launch errors: a remote that is gone fails the run (remote_unavailable)
// and pauses the schedule; a restic repository that is gone the same with
// repository_unavailable, one whose data is being deleted with
// repository_deleting; an invalid definition the same with
// invalid_definition; a worker that disappeared puts the run back to
// pending (the next tick decides again), or fails it (worker_removed, no
// pause) if a later occurrence opened a pending run meanwhile; any other
// error leaves the run launching, retried on the next tick with the same
// job ID.
//
// # No double runs
//
// Across restarts and concurrent schedulers (two controllers): a fire is a
// compare-and-swap on NextRunAt; a schedule has at most one pending run (a
// unique index); the pending -> launching compare-and-swap reserves the job
// ID once; and jobs.Create is idempotent by ID. A crash between any two
// steps is retried from the stored state with the same job ID.
//
// Allowed dependencies: the standard library, internal/domain, the app
// services' types (internal/app/...), internal/jobkind and the generated
// protobuf code; collaborators are the ports in ports.go.
package scheduler
