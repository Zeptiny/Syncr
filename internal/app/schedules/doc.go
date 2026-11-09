// Package schedules owns schedules: a user's job definition run on a cron
// expression in a time zone (internal/cronexpr), on one worker or on any
// worker with a label, with a missed-run and an overlap policy and the
// contacts to notify; and their runs, one per occurrence (or several merged
// ones), which the scheduler (internal/controller/scheduler) fires and
// launches (Milestone 4).
//
// # Rules
//
//   - Schedules are strictly private to their owner, admins included:
//     another user's schedule is ErrNotFound for every use case. The jobs
//     a schedule launches belong to its owner.
//   - A name is 1 to 64 characters, unique per owner, case-insensitively.
//   - The definition is checked by the API exactly as RunJob checks it
//     (jobkind.Check, then Marshal); this package receives it encoded with
//     its references (Input) and checks that the remotes are the owner's
//     live remotes (RemoteChecker) and the contacts the owner's
//     (ContactChecker). Schedules store a definition of any registered job
//     kind.
//   - The target is exactly one of a worker ID, which must exist and not be
//     removed when the schedule is saved (WorkerChecker), or a label
//     (domain.ValidLabel), which no worker needs to have yet.
//   - Each contact link has a non-empty subset of domain.ScheduleEvents. A
//     scheduled job notifies its schedule's contacts only, never the
//     contacts' JobEvents.
//   - SetEnabled needs no version: pausing sets NextRunAt to zero and
//     cancels the open runs (no notification; a launching run gets a
//     cancel request, Run.CancelRequested, for the scheduler to cancel its
//     job and then the run); resuming clears PausedReason
//     and sets NextRunAt to the next occurrence after now. Saving
//     recomputes NextRunAt only when Cron, Timezone or Enabled changed.
//   - A definition on a restic repository (Milestone 5) needs a live
//     repository of the owner whose data is not being deleted
//     (RepositoryChecker; ErrUnknownRepository, ErrRepositoryDeleting).
//     Deleting a repository's data pauses its schedules
//     (ReasonRepositoryDeleting).
//   - Runs that cannot launch for a permanent reason (worker removed,
//     definition no longer valid, a remote or the repository gone, owner
//     disabled) fail and pause the schedule with PausedReason set (EndRun
//     with pause).
//   - A schedule has at most one pending run: a later occurrence due
//     before its deadline is merged into it (Coalesced), so a desktop that
//     slept runs once when it wakes up. The run's deadline is fixed when it
//     opens: an occurrence due at or after it supersedes the run (missed,
//     MissedReason) and opens its own, so a worker that stays offline
//     produces missed runs instead of one run waiting forever. A manual
//     request merging into the pending run extends its deadline to the
//     request's own. A launching run absorbs nothing: an occurrence due
//     meanwhile opens its own pending run, which the overlap policy holds
//     or launches.
//
// # Policies (policy.go)
//
// Pure functions decide fires and deadlines:
//
//   - Fire, when NextRunAt <= now (PlanFire): (last, n) :=
//     expr.Latest(NextRunAt - 1ns, now, MaxCoalesce); the run is for last
//     with Coalesced n - 1, and NewNext = expr.Next(now). Occurrences that
//     passed while the controller was down are never run one by one; those
//     whose deadline had passed when they fired are missed with
//     ReasonControllerUnavailable instead (FirePlan.Late, LateFor).
//   - Deadline: run_late is ScheduledFor + MaxDelay, or the next
//     occurrence after ScheduledFor for MaxDelay 0 (ManualUntilNext if
//     there is none); skip is ScheduledFor + SkipGrace. A manual run's
//     deadline counts from now (ManualDeadline).
//   - Overlap at fire time (SkipsOverlap): skip records a missed run
//     (ReasonOverlap) if a run is active (HasActiveRun), else opens or
//     merges a run; queue and allow open or merge a run. At launch queue
//     (and skip) wait for the active run, allow does not (WaitsForActive).
//   - A pending run past its deadline is missed: ReasonOverlap if it
//     waited behind an active run, else ReasonWorkerOffline (MissedReason).
//   - Permanent launch failures pause the schedule (AutoPauses); a failed
//     run of a disabled owner is not notified (Notifies).
//
// # Service
//
// The Service is constructed as New(Deps{Repo Repository; Remotes
// RemoteChecker; Contacts ContactChecker; Workers WorkerChecker;
// Repositories RepositoryChecker; Waker Waker; Clock Clock; IDs
// IDGenerator; Logger *slog.Logger}, Config). Its
// user-facing methods are scoped by the actor (owner only, admins
// included; ErrNotFound otherwise): Create, Get, List, Update, SetEnabled,
// Delete, RunNow, Runs, LatestRuns, and the dashboard's Upcoming,
// RecentProblems and CountMissed. Preview needs no actor; Names returns
// references of any owner's schedules (a job's schedule).
//
// The scheduler-facing methods take no actor (the scheduler acts for the
// owner): FireDue fires the due schedules (ListDueSchedules, then
// FireSchedule per schedule, a compare-and-swap); OpenRuns, Load and
// HasActiveRun read what a launch needs; MarkLaunching (pending ->
// launching, reserving the job ID), MarkLaunched (launching -> launched)
// and EndRun (to missed, failed or pending again, optionally pausing the
// schedule) are compare-and-swaps of the run's status, so two ticks (or
// two controllers) never launch a run twice.
//
// Allowed dependencies: the standard library, internal/domain and
// internal/cronexpr; every other collaborator is a port declared in
// ports.go.
package schedules
