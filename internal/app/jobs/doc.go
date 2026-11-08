// Package jobs owns the job lifecycle: creating jobs (an explicit worker and
// a secret-free job definition, owned by a user), reading and listing them,
// canceling them, live progress, and every status change the dispatcher
// derives from the worker link.
//
// Every status change goes through this service, which
//
//   - checks it against the domain state machine (domain.JobStatus) and the
//     link preconditions (the job belongs to the calling worker and, for a
//     dispatched or running job, to the worker epoch that accepted it);
//   - stores it with an optimistic, version-checked update
//     (Repository.UpdateJob), retrying on a lost race, so concurrent changes
//     from the API and the dispatcher serialize per job;
//   - publishes the new job to the live broker, wakes the worker's command
//     long poll when its commands change (re-queue, cancel request), and
//     tells the notifier when a job reaches a terminal status (it only
//     wakes the notification planner and the scheduler; notifications are
//     planned durably from the store).
//
// Lifecycle calls are idempotent: a call whose precondition no longer holds
// (a duplicate event, a lost race, a job already finished) changes nothing
// and reports changed == false without an error.
//
// # Ownership
//
// A job belongs to the actor who created it. The API use cases (Create,
// Get, List, Cancel, Watch, Samples) take the actor: a user sees and
// cancels only their own jobs, an admin any job (List returns the admin's
// own jobs unless ListFilter.AllUsers is set). Another user's job is
// domain.ErrNotFound, so its existence does not leak.
//
// # Definitions
//
// A job stores its definition (see internal/jobkind) already encoded and
// checked by the caller; this package does not interpret it. It contains
// no secrets: the dispatcher builds the worker's JobSpec, with the remotes'
// credentials, when it sends the job. Create checks that the remotes the
// definition refers to are live remotes of the actor (RemoteChecker), keeps
// their IDs (Job.RemoteIDs, for ListFilter.RemoteID and display) and builds
// the job's search text from the kind, the definition's search terms and
// the remotes' names (ListFilter.Query).
//
// # Progress
//
// Progress samples are cumulative counters. Every sample is published live;
// one per Config.SampleInterval per job is stored (rate limiting happens in
// memory before storage), plus the final counters when the job ends. Rates
// are derived by readers (delta bytes / delta time).
//
// # Schedules and the dashboard (Milestone 4)
//
// A job launched by a schedule carries its ScheduleID (ListFilter.ScheduleID
// lists a schedule's jobs) and may have an ID preset by the scheduler
// (CreateParams.ID), which makes a retried launch idempotent. The dashboard
// use cases live in activity.go:
//
//	// Activity counts the finished jobs of f by bucket (every bucket of
//	// [f.From, f.Until), empty ones included): the actor's jobs, or every
//	// user's for an admin with f.AllUsers.
//	Activity(ctx, actor domain.Actor, f ActivityFilter) ([]ActivityBucket, error)
//	// CountUnfinished counts the queued, dispatched and running jobs the
//	// same way, by status.
//	CountUnfinished(ctx, actor domain.Actor, allUsers bool) (map[domain.JobStatus]int64, error)
//
// Allowed dependencies: the standard library and internal/domain; every
// other collaborator is a port declared in ports.go.
package jobs
