package schedules_test

import (
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/cronexpr"
)

func mustExpr(t *testing.T, expr, tz string) *cronexpr.Expr {
	t.Helper()
	e, err := cronexpr.Parse(expr, tz)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPlanFireCoalesces(t *testing.T) {
	t.Parallel()
	hourly := mustExpr(t, "0 * * * *", "UTC")
	// Long enough for no occurrence to pass its deadline (TestPlanFireLate).
	late := schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: schedules.MaxMaxDelay}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC) }
	tests := []struct {
		name          string
		e             *cronexpr.Expr
		next, now     time.Time
		wantScheduled time.Time
		wantCoalesced int
		wantNext      time.Time
	}{
		{"on time", hourly, at(10, 0), at(10, 0), at(10, 0), 0, at(11, 0)},
		{"a tick late", hourly, at(10, 0), at(10, 0).Add(9 * time.Second), at(10, 0), 0, at(11, 0)},
		// The controller was down from 09:30 to 15:30: one run for 15:00,
		// the five earlier occurrences counted.
		{"after downtime", hourly, at(10, 0), at(15, 30), at(15, 0), 5, at(16, 0)},
		{"exactly at the next", hourly, at(10, 0), at(11, 0), at(11, 0), 1, at(12, 0)},
		// A next run that is no longer an occurrence (the zone's rules
		// changed) still fires once.
		{"stale next", hourly, at(10, 7), at(10, 30), at(10, 7), 0, at(11, 0)},
		// More occurrences than MaxCoalesce: the count is capped, the run is
		// still for the latest.
		{"long downtime", mustExpr(t, "* * * * *", "UTC"), at(0, 0), at(23, 59), at(23, 59), schedules.MaxCoalesce - 1, at(23, 59).Add(time.Minute)},
		// Downtime across a spring-forward gap: the gap's occurrences run
		// once, at the transition.
		{"across a gap", mustExpr(t, "*/30 * * * *", "Europe/Berlin"), time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC),
			time.Date(2026, 3, 29, 1, 40, 0, 0, time.UTC), time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), 2,
			time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		p := schedules.PlanFire(tt.e, tt.next, tt.now, late, schedules.OverlapSkip, false)
		if !p.ScheduledFor.Equal(tt.wantScheduled) || p.Coalesced != tt.wantCoalesced || !p.NewNext.Equal(tt.wantNext) || p.Skip ||
			p.Late || !p.LateFor.IsZero() {
			t.Errorf("%s: PlanFire = %+v, want scheduled %v, coalesced %d, next %v", tt.name, p, tt.wantScheduled, tt.wantCoalesced, tt.wantNext)
		}
		if !p.Deadline.Equal(p.ScheduledFor.Add(late.MaxDelay)) {
			t.Errorf("%s: deadline %v, want the scheduled time + max delay", tt.name, p.Deadline)
		}
	}
}

// TestPlanFireLate: occurrences whose deadline passed before they fired
// (the controller was down past it) are missed (controller unavailable):
// the whole fire (Late) or the earlier ones (LateFor), never the ones that
// may still start.
func TestPlanFireLate(t *testing.T) {
	t.Parallel()
	hourly := mustExpr(t, "0 * * * *", "UTC")
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC) }
	untilNext := schedules.MissedPolicy{Action: schedules.MissedRunLate}
	twoHours := schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 2 * time.Hour}
	skip := schedules.MissedPolicy{Action: schedules.MissedSkip}
	tests := []struct {
		name          string
		missed        schedules.MissedPolicy
		next, now     time.Time
		late          bool
		lateFor       time.Time // zero: none
		lateCoalesced int
		coalesced     int
	}{
		{"on time", untilNext, at(10, 0), at(10, 0), false, time.Time{}, 0, 0},
		// Until the next occurrence: every occurrence but the latest is
		// past its deadline (the occurrence after it).
		{"until next, down 09:30-15:30", untilNext, at(10, 0), at(15, 30), false, at(14, 0), 4, 0},
		{"until next, one occurrence", untilNext, at(10, 0), at(10, 30), false, time.Time{}, 0, 0},
		// At 11:00 sharp the 10:00 run may still start (its deadline is
		// now): it is coalesced, not missed.
		{"until next, at the deadline", untilNext, at(10, 0), at(11, 0), false, time.Time{}, 0, 1},
		// Two hours: 10:00 to 13:00 are past their deadline at 15:30, 14:00
		// and 15:00 may still start.
		{"two hours", twoHours, at(10, 0), at(15, 30), false, at(13, 0), 3, 1},
		{"two hours, short downtime", twoHours, at(10, 0), at(11, 30), false, time.Time{}, 0, 1},
		// Skip: five minutes of grace.
		{"skip, within the grace", skip, at(10, 0), at(10, 4), false, time.Time{}, 0, 0},
		{"skip, down past the grace", skip, at(10, 0), at(10, 6), true, time.Time{}, 0, 0},
		{"skip, down for hours", skip, at(10, 0), at(15, 30), true, time.Time{}, 0, 5},
	}
	for _, tt := range tests {
		p := schedules.PlanFire(hourly, tt.next, tt.now, tt.missed, schedules.OverlapSkip, false)
		if p.Late != tt.late || !p.LateFor.Equal(tt.lateFor) || p.LateCoalesced != tt.lateCoalesced || p.Coalesced != tt.coalesced {
			t.Errorf("%s: PlanFire = %+v, want late %v, late for %v (+%d), coalesced %d",
				tt.name, p, tt.late, tt.lateFor, tt.lateCoalesced, tt.coalesced)
		}
	}
}

func TestDeadlines(t *testing.T) {
	t.Parallel()
	hourly := mustExpr(t, "0 * * * *", "UTC")
	// Feb 29 has no occurrence within five years of 2097-03-01.
	never := mustExpr(t, "0 0 29 2 *", "UTC")
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	tail := time.Date(2097, 3, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		e      *cronexpr.Expr
		missed schedules.MissedPolicy
		from   time.Time
		manual bool
		want   time.Time
	}{
		{"run late with max delay", hourly, schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 3 * time.Hour}, at, false, at.Add(3 * time.Hour)},
		{"run late until next", hourly, schedules.MissedPolicy{Action: schedules.MissedRunLate}, at, false, at.Add(time.Hour)},
		{"run late until next, none left", never, schedules.MissedPolicy{Action: schedules.MissedRunLate}, tail, false, tail.Add(schedules.ManualUntilNext)},
		{"skip", hourly, schedules.MissedPolicy{Action: schedules.MissedSkip}, at, false, at.Add(schedules.SkipGrace)},
		{"skip ignores max delay", hourly, schedules.MissedPolicy{Action: schedules.MissedSkip, MaxDelay: time.Hour}, at, false, at.Add(schedules.SkipGrace)},
		// Manual runs count from the request.
		{"manual with max delay", hourly, schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 90 * time.Minute}, at.Add(17 * time.Minute), true, at.Add(107 * time.Minute)},
		{"manual until next", hourly, schedules.MissedPolicy{Action: schedules.MissedRunLate}, at.Add(17 * time.Minute), true, at.Add(time.Hour)},
		{"manual until next, none left", never, schedules.MissedPolicy{Action: schedules.MissedRunLate}, tail, true, tail.Add(24 * time.Hour)},
		{"manual skip", hourly, schedules.MissedPolicy{Action: schedules.MissedSkip}, at.Add(17 * time.Minute), true, at.Add(22 * time.Minute)},
	}
	for _, tt := range tests {
		got := schedules.Deadline(tt.e, tt.missed, tt.from)
		if tt.manual {
			got = schedules.ManualDeadline(tt.e, tt.missed, tt.from)
		}
		if !got.Equal(tt.want) {
			t.Errorf("%s: deadline = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestOverlapMatrix covers the overlap policies against an active run (the
// fire side; the launch side is WaitsForActive).
func TestOverlapMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		overlap             schedules.Overlap
		active              bool
		wantSkip, wantWaits bool
		wantMissedReason    string
	}{
		{schedules.OverlapSkip, true, true, true, schedules.ReasonOverlap},
		{schedules.OverlapSkip, false, false, true, schedules.ReasonWorkerOffline},
		{"", true, true, true, schedules.ReasonOverlap}, // the default is skip
		{schedules.OverlapQueue, true, false, true, schedules.ReasonOverlap},
		{schedules.OverlapQueue, false, false, true, schedules.ReasonWorkerOffline},
		{schedules.OverlapAllow, true, false, false, schedules.ReasonWorkerOffline},
		{schedules.OverlapAllow, false, false, false, schedules.ReasonWorkerOffline},
	}
	hourly := mustExpr(t, "0 * * * *", "UTC")
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		if got := schedules.SkipsOverlap(tt.overlap, tt.active); got != tt.wantSkip {
			t.Errorf("SkipsOverlap(%q, %v) = %v", tt.overlap, tt.active, got)
		}
		if got := schedules.WaitsForActive(tt.overlap); got != tt.wantWaits {
			t.Errorf("WaitsForActive(%q) = %v", tt.overlap, got)
		}
		if got := schedules.MissedReason(tt.overlap, tt.active); got != tt.wantMissedReason {
			t.Errorf("MissedReason(%q, %v) = %q", tt.overlap, tt.active, got)
		}
		if p := schedules.PlanFire(hourly, at, at, schedules.MissedPolicy{Action: schedules.MissedRunLate}, tt.overlap, tt.active); p.Skip != tt.wantSkip {
			t.Errorf("PlanFire(%q, active %v).Skip = %v", tt.overlap, tt.active, p.Skip)
		}
	}
}

func TestMissedMessage(t *testing.T) {
	t.Parallel()
	byWorker := schedules.Schedule{Target: schedules.Target{Label: ""}}
	byLabel := schedules.Schedule{Target: schedules.Target{Label: "nas"}}
	for _, tt := range []struct {
		sc     schedules.Schedule
		reason string
		want   string
	}{
		{byWorker, schedules.ReasonWorkerOffline, "the worker was offline until the run's deadline"},
		{byLabel, schedules.ReasonWorkerOffline, `no online worker with the label "nas" took the run before its deadline`},
		{byLabel, schedules.ReasonOverlap, "an earlier run of the schedule was still active at the deadline"},
	} {
		if got := schedules.MissedMessage(tt.sc, tt.reason); got != tt.want {
			t.Errorf("MissedMessage(%q, %q) = %q, want %q", tt.sc.Target.Label, tt.reason, got, tt.want)
		}
	}
}

func TestAutoPauseReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason        string
		pause, notify bool
	}{
		{schedules.ReasonWorkerRemoved, true, true},
		{schedules.ReasonInvalidDefinition, true, true},
		{schedules.ReasonRemoteUnavailable, true, true},
		{schedules.ReasonOwnerDisabled, true, false},
		{schedules.ReasonNoWorkerWithLabel, false, true},
		{schedules.ReasonWorkerOffline, false, true},
		{schedules.ReasonOverlap, false, true},
		{schedules.ReasonControllerUnavailable, false, true},
		{schedules.ReasonDisabled, false, true},
	}
	for _, tt := range tests {
		if got := schedules.AutoPauses(tt.reason); got != tt.pause {
			t.Errorf("AutoPauses(%q) = %v, want %v", tt.reason, got, tt.pause)
		}
		if got := schedules.Notifies(tt.reason); got != tt.notify {
			t.Errorf("Notifies(%q) = %v, want %v", tt.reason, got, tt.notify)
		}
	}
}
