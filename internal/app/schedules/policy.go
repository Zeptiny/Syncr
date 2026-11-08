package schedules

import (
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
}

// PlanFire plans the fire of a schedule whose NextRunAt next is due at now
// (next <= now): one run for the latest occurrence in [next, now], counting
// the earlier ones as coalesced, so downtime never replays occurrences one
// by one. active tells whether a run of the schedule is active (launching,
// or its job queued, dispatched or running; only read for OverlapSkip).
func PlanFire(e *cronexpr.Expr, next, now time.Time, missed MissedPolicy, overlap Overlap, active bool) FirePlan {
	last, n := e.Latest(next.Add(-time.Nanosecond), now, MaxCoalesce)
	if n == 0 {
		// next is not an occurrence of the expression any more (the zone's
		// rules changed since it was computed): it still fires, once.
		last, n = next, 1
	}
	return FirePlan{
		ScheduledFor: last,
		Coalesced:    n - 1,
		NewNext:      e.Next(now),
		Deadline:     Deadline(e, missed, last),
		Skip:         SkipsOverlap(overlap, active),
	}
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

// AutoPauses reports whether a run that failed for reason pauses its
// schedule: the permanent launch failures (the worker was removed, the
// definition is no longer valid, a remote is gone, the owner is disabled),
// which every later run would hit too. A missing label worker does not
// pause (a worker may get the label), nor do missed runs.
func AutoPauses(reason string) bool {
	switch reason {
	case ReasonWorkerRemoved, ReasonInvalidDefinition, ReasonRemoteUnavailable, ReasonOwnerDisabled:
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
