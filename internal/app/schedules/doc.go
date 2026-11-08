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
//     cancels the open run (no notification); resuming clears PausedReason
//     and sets NextRunAt to the next occurrence after now. Saving
//     recomputes NextRunAt only when Cron, Timezone or Enabled changed.
//   - Runs that cannot launch for a permanent reason (worker removed,
//     definition no longer valid, a remote gone, owner disabled) fail and
//     pause the schedule with PausedReason set (EndRun with pause).
//   - A schedule has at most one open run (pending or launching): a later
//     occurrence is merged into it (Coalesced, the later deadline), so a
//     desktop that slept runs once when it wakes up.
//
// # Policies (policy.go)
//
// Pure functions decide fires and deadlines:
//
//   - Fire, when NextRunAt <= now: (last, n) := expr.Latest(NextRunAt -
//     1ns, now, MaxCoalesce); the run is for last with Coalesced n - 1,
//     and NewNext = expr.Next(now).
//   - Deadline: run_late is ScheduledFor + MaxDelay, or the next
//     occurrence after ScheduledFor for MaxDelay 0; skip is ScheduledFor +
//     SkipGrace. A manual run's deadline counts from now (MaxDelay, or
//     ManualUntilNext when "until next" has no next occurrence).
//   - Overlap at fire time: skip records a missed run (ReasonOverlap) if a
//     run is active (HasActiveRun), else opens or merges a run; queue and
//     allow open or merge a run (launch waits for, or ignores, the active
//     one).
//
// # Service (Milestone 4, track A)
//
// The Service is constructed as New(Deps{Repo Repository; Remotes
// RemoteChecker; Contacts ContactChecker; Workers WorkerChecker; Waker
// Waker; Clock Clock; IDs IDGenerator; Logger *slog.Logger}, Config). Its
// methods, all scoped by the actor (owner only; ErrNotFound otherwise):
//
//	// Create validates and stores a new schedule of actor.
//	Create(ctx, actor domain.Actor, in Input) (Schedule, error)
//	// Get returns one of actor's schedules.
//	Get(ctx, actor domain.Actor, id domain.ScheduleID) (Schedule, error)
//	// List returns actor's schedules matching f (f.OwnerID is forced to
//	// the actor), by name.
//	List(ctx, actor domain.Actor, f ListFilter) ([]Schedule, error)
//	// Update changes one of actor's schedules; version is the one the
//	// caller read (ErrConflict if it changed).
//	Update(ctx, actor domain.Actor, id domain.ScheduleID, version int64, in Input) (Schedule, error)
//	// SetEnabled pauses or resumes one of actor's schedules.
//	SetEnabled(ctx, actor domain.Actor, id domain.ScheduleID, enabled bool) (Schedule, error)
//	// Delete deletes one of actor's schedules and its runs.
//	Delete(ctx, actor domain.Actor, id domain.ScheduleID) error
//	// RunNow adds a manual run (merged into an open run, if any).
//	// ErrFailedPrecondition if the overlap policy is skip and a run is
//	// active.
//	RunNow(ctx, actor domain.Actor, id domain.ScheduleID) (Run, error)
//	// Runs returns a page of one of actor's schedules' runs, newest first
//	// (before: cursor; limit 0 means DefaultPageSize).
//	Runs(ctx, actor domain.Actor, id domain.ScheduleID, before domain.ScheduleRunID, limit int) (RunPage, error)
//	// LatestRuns returns the newest run of each of actor's schedules ids.
//	LatestRuns(ctx, actor domain.Actor, ids []domain.ScheduleID) (map[domain.ScheduleID]Run, error)
//	// Preview checks an expression and returns its next count (1 to 20,
//	// 0 means 5) occurrences after after (zero: now).
//	Preview(cron, timezone string, after time.Time, count int) (Preview, error)
//	// Names returns display references of ids, of any owner (jobs'
//	// schedules).
//	Names(ctx, ids []domain.ScheduleID) (map[domain.ScheduleID]Ref, error)
//
//	// Dashboard: actor's next occurrences, latest missed or failed runs,
//	// and the count of missed runs scheduled in [from, until).
//	Upcoming(ctx, actor domain.Actor, limit int) ([]Upcoming, error)
//	RecentProblems(ctx, actor domain.Actor, limit int) ([]Run, error)
//	CountMissed(ctx, actor domain.Actor, from, until time.Time) (int64, error)
//
//	// Scheduler-facing (no actor; the scheduler acts for the owner):
//	// FireDue fires the due schedules (ListDueSchedules, FireSchedule) and
//	// returns how many fired.
//	FireDue(ctx) (int, error)
//	OpenRuns(ctx, limit int) ([]Run, error)
//	HasActiveRun(ctx, id domain.ScheduleID) (bool, error)
//	// MarkLaunching reserves jobID on a pending run (pending -> launching).
//	MarkLaunching(ctx, run Run, jobID domain.JobID, workerID domain.WorkerID) (Run, error)
//	// MarkLaunched records that the run's job exists (launching ->
//	// launched).
//	MarkLaunched(ctx, run Run) (Run, error)
//	// EndRun ends an open run as missed, failed or pending again (status),
//	// pausing the schedule with reason if pause.
//	EndRun(ctx, run Run, status RunStatus, reason, msg string, pause bool) (Run, error)
//
// Allowed dependencies: the standard library, internal/domain and
// internal/cronexpr; every other collaborator is a port declared in
// ports.go.
package schedules
