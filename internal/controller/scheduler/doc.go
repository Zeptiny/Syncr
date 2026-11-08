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
//     the schedule, an offline or never connected one keeps it waiting; a
//     label without workers fails it (no pause), and otherwise the least
//     busy online worker with the label that runs the kind is chosen (fewest
//     unfinished jobs, then name), or the run waits.
//  3. launches in two idempotent phases: MarkLaunching reserves a job ID on
//     the run (pending -> launching, a compare-and-swap), jobs.Create
//     creates the job with that ID (ErrAlreadyExists counts as success),
//     MarkLaunched closes the run (launching -> launched). A run found
//     launching (a crash or an error after the reservation) is completed
//     with its reserved ID on the next tick, whatever its deadline: the
//     launch was decided in time.
//
// Launch errors: a remote that is gone fails the run (remote_unavailable)
// and pauses the schedule; an invalid definition the same with
// invalid_definition; a worker that disappeared puts the run back to
// pending (the next tick decides again); any other error leaves the run
// launching, retried on the next tick with the same job ID.
//
// # No double runs
//
// Across restarts and concurrent schedulers (two controllers): a fire is a
// compare-and-swap on NextRunAt; a schedule has at most one open run (a
// unique index); the pending -> launching compare-and-swap reserves the job
// ID once; and jobs.Create is idempotent by ID. A crash between any two
// steps is retried from the stored state with the same job ID.
//
// Allowed dependencies: the standard library, internal/domain, the app
// services' types (internal/app/...), internal/jobkind and the generated
// protobuf code; collaborators are the ports in ports.go.
package scheduler
