package schedules

import (
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/cronexpr"
)

// This file holds the scheduling policies as pure functions (no I/O, no
// clock): what a fire records, when a run's deadline is, what overlapping
// runs do, and which launch failures pause a schedule. The service and the
// scheduler (internal/controller/scheduler) apply them.

// FirePlan is what one fire of a due schedule records.
type FirePlan struct {
	// ScheduledFor is the latest due occurrence; Coalesced counts the
	// earlier due ones folded into the same run (they passed while the
	// controller was down, or within one tick).
	ScheduledFor time.Time
	Coalesced    int
	// NewNext is the schedule's next occurrence after now (zero: none).
	NewNext time.Time
	// Deadline is the run's deadline (see Deadline).
	Deadline time.Time
	// Skip records the run as missed (reason overlap) instead of opening
	// it: the overlap policy is skip and a run is active.
	Skip bool
	// Late records the run as missed (ReasonControllerUnavailable) instead
	// of opening it: its deadline had passed when it fired, so the
	// controller was down (or not scheduling) from before the deadline
	// until now. It takes precedence over Skip.
	Late bool
	// LateFor is the latest of the earlier due occurrences whose deadline
	// had passed when the run fired (zero: none); LateCoalesced counts the
	// ones before it. They are not in Coalesced: they are recorded as one
	// missed run for LateFor (ReasonControllerUnavailable), so downtime
	// that outlasted their deadlines is reported, as a worker that stayed
	// offline would be.
	LateFor       time.Time
	LateCoalesced int
}

// PlanFire plans the fire of a schedule whose NextRunAt next is due at now
// (next <= now): one run for the latest occurrence in [next, now], counting
// the earlier ones as coalesced, so downtime never replays occurrences one
// by one. Occurrences whose deadline had already passed when they fired
// (the controller was down past it) are missed instead (Late, LateFor).
// active tells whether a run of the schedule is active (launching, or its
// job queued, dispatched or running; only read for OverlapSkip).
func PlanFire(e *cronexpr.Expr, next, now time.Time, missed MissedPolicy, overlap Overlap, active bool) FirePlan {
	last, n := e.Latest(next.Add(-time.Nanosecond), now, MaxCoalesce)
	if n == 0 {
		// next is not an occurrence of the expression any more (the zone's
		// rules changed since it was computed): it still fires, once.
		last, n = next, 1
	}
	p := FirePlan{
		ScheduledFor: last,
		Coalesced:    n - 1,
		NewNext:      e.Next(now),
		Deadline:     Deadline(e, missed, last),
		Skip:         SkipsOverlap(overlap, active),
	}
	if now.After(p.Deadline) {
		p.Late = true
		return p
	}
	if lateFor, k := latePrefix(e, missed, next, last, now); k > 0 {
		p.LateFor, p.LateCoalesced = lateFor, k-1
		p.Coalesced = max(n-1-k, 0)
	}
	return p
}

// latePrefix returns the latest occurrence in [next, last) whose deadline
// is before now, and how many there are (at most MaxCoalesce). Deadlines
// grow with their occurrences, so these are the occurrences before a cut.
func latePrefix(e *cronexpr.Expr, missed MissedPolicy, next, last, now time.Time) (time.Time, int) {
	var cut time.Time // occurrences before cut have their deadline before now
	switch {
	case missed.Action == MissedSkip:
		cut = now.Add(-SkipGrace)
	case missed.MaxDelay > 0:
		cut = now.Add(-missed.MaxDelay)
	default:
		// Until the next occurrence: the deadline of an occurrence is the
		// one after it, which is before now exactly for the occurrences
		// before the latest one before now.
		cut, _ = e.Latest(next.Add(-time.Nanosecond), now.Add(-time.Nanosecond), 1)
	}
	if cut.After(last) {
		cut = last
	}
	if !cut.After(next) {
		return time.Time{}, 0
	}
	return e.Latest(next.Add(-time.Nanosecond), cut.Add(-time.Nanosecond), MaxCoalesce)
}

// Deadline returns the latest time a run scheduled for scheduledFor may
// still start: for run_late scheduledFor + MaxDelay, or the next occurrence
// after scheduledFor when MaxDelay is 0 ("until the next occurrence";
// ManualUntilNext after scheduledFor if there is none); for skip
// scheduledFor + SkipGrace.
func Deadline(e *cronexpr.Expr, missed MissedPolicy, scheduledFor time.Time) time.Time {
	if missed.Action == MissedSkip {
		return scheduledFor.Add(SkipGrace)
	}
	if missed.MaxDelay > 0 {
		return scheduledFor.Add(missed.MaxDelay)
	}
	if next := e.Next(scheduledFor); !next.IsZero() {
		return next
	}
	return scheduledFor.Add(ManualUntilNext)
}

// ManualDeadline returns the deadline of a run requested at now (RunNow):
// the same policy counted from now, except that "until the next
// occurrence" without a next occurrence allows ManualUntilNext.
func ManualDeadline(e *cronexpr.Expr, missed MissedPolicy, now time.Time) time.Time {
	return Deadline(e, missed, now)
}

// SkipsOverlap reports whether an occurrence that is due while a run is
// active is recorded as missed (reason overlap) instead of opening (or
// merging into) a run: only with OverlapSkip. queue and allow always open
// or merge; at launch queue waits for the active run and allow does not
// (WaitsForActive).
func SkipsOverlap(overlap Overlap, active bool) bool {
	return active && overlap.orDefault() == OverlapSkip
}

// WaitsForActive reports whether a pending run waits while another run of
// its schedule is active: every policy but allow.
func WaitsForActive(overlap Overlap) bool { return overlap.orDefault() != OverlapAllow }

// MissedReason is the reason code of a pending run that reached its
// deadline: overlap when it waited behind an active run (active, and the
// policy waits), otherwise worker_offline (no worker took it in time).
func MissedReason(overlap Overlap, active bool) string {
	if active && WaitsForActive(overlap) {
		return ReasonOverlap
	}
	return ReasonWorkerOffline
}

// MissedMessage explains a run missed for reason (MissedReason) to the
// user.
func MissedMessage(sc Schedule, reason string) string {
	switch {
	case reason == ReasonOverlap:
		return "an earlier run of the schedule was still active at the deadline"
	case reason == ReasonControllerUnavailable:
		return "the controller was not running from before the run's deadline until after it"
	case sc.Target.Label != "":
		return fmt.Sprintf("no online worker with the label %q took the run before its deadline", sc.Target.Label)
	default:
		return "the worker was offline until the run's deadline"
	}
}

// AutoPauses reports whether a run that failed for reason pauses its
// schedule: the permanent launch failures (the worker was removed, the
// definition is no longer valid, a remote or the repository is gone or its
// data is being deleted, the owner is disabled), which every later run
// would hit too. A missing label worker does not pause (a worker may get
// the label), nor do missed runs.
func AutoPauses(reason string) bool {
	switch reason {
	case ReasonWorkerRemoved, ReasonInvalidDefinition, ReasonRemoteUnavailable, ReasonOwnerDisabled,
		ReasonRepositoryUnavailable, ReasonRepositoryDeleting:
		return true
	}
	return false
}

// Notifies reports whether a missed or failed run with reason is
// notified: every one except a run that failed because its owner is
// disabled (nobody to tell). The notification planner applies the same
// rule to the stored runs.
func Notifies(reason string) bool { return reason != ReasonOwnerDisabled }

// orDefault maps the empty policy to OverlapSkip.
func (o Overlap) orDefault() Overlap {
	if o == "" {
		return OverlapSkip
	}
	return o
}
