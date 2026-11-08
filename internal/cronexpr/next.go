package cronexpr

import (
	"math/bits"
	"time"
)

// This file computes occurrences with the daylight saving time rules of the
// package documentation.
//
// A wall time W (year, month, day, hour, minute in the expression's zone) is
// handled as a "naive" time: W's fields in UTC, which has no transitions, so
// the calendar iteration (nextWall, prevWall) is plain arithmetic. resolve
// maps W to the instant it runs at:
//
//	R(W) = the first instant at which the zone's wall clock shows W or later.
//
// That is W's instant when W exists once, its earlier instant when it exists
// twice (fall-back), and the transition instant when it does not exist (the
// spring-forward gap). R never decreases as W increases, so the occurrences
// in instant order are R of the matching wall times in calendar order,
// without the repeats: Next skips a wall time whose instant is not after
// the previous one, which runs each wall time at most once and collapses a
// gap's occurrences (and the first wall time after it) into one run.

// maxOffset bounds the absolute UTC offset of any zone (real ones stay
// within -12h and +14h).
const maxOffset = 15 * time.Hour

// Next returns the first occurrence strictly after after, in the
// expression's zone, or the zero time if there is none within five years.
func (e *Expr) Next(after time.Time) time.Time {
	w := naiveMinute(after.In(e.loc))
	limit := w.AddDate(horizonYears, 0, 0)
	// R(w) <= after for the wall time of after itself (the clock shows it at
	// after), so the candidates are the later wall times.
	for {
		var ok bool
		if w, ok = e.nextWall(w, limit); !ok {
			return time.Time{}
		}
		if r := e.resolve(w); r.After(after) {
			return r.In(e.loc)
		}
	}
}

// NextN returns the first n occurrences after after, oldest first (fewer
// if the five-year horizon ends first). n is capped at MaxPreview.
func (e *Expr) NextN(after time.Time, n int) []time.Time {
	n = min(max(n, 0), MaxPreview)
	out := make([]time.Time, 0, n)
	for len(out) < n {
		t := e.Next(after)
		if t.IsZero() {
			break
		}
		out = append(out, t)
		after = t
	}
	return out
}

// Latest returns the latest occurrence in (after, until] and how many
// occurrences that interval holds, counting at most maxCount (at least 1).
// last is zero and count 0 if there is none. The scheduler uses it to fire
// one run for the occurrences that passed while the controller was down.
func (e *Expr) Latest(after, until time.Time, maxCount int) (last time.Time, count int) {
	maxCount = max(maxCount, 1)
	t := after
	for count < maxCount {
		next := e.Next(t)
		if next.IsZero() || next.After(until) {
			return last, count
		}
		last, count, t = next, count+1, next
	}
	// The count is capped; the latest occurrence is still exact.
	if l := e.latestAtOrBefore(until); l.After(last) {
		last = l
	}
	return last, count
}

// latestAtOrBefore returns the latest occurrence at or before until, or the
// zero time if there is none within five years before it.
func (e *Expr) latestAtOrBefore(until time.Time) time.Time {
	wu := naiveMinute(until.In(e.loc))
	// Wall times after until's can still run at or before until (the
	// repeated hour of a fall-back, which run at their first pass); they
	// follow every earlier wall time, so the latest of them wins.
	var last time.Time
	for w := wu; ; {
		var ok bool
		if w, ok = e.nextWall(w, wu.Add(2*24*time.Hour)); !ok {
			break
		}
		r := e.resolve(w)
		if r.After(until) {
			break
		}
		last = r
	}
	if !last.IsZero() {
		return last.In(e.loc)
	}
	w, ok := e.prevWall(wu, wu.AddDate(-horizonYears, 0, 0))
	if !ok {
		return time.Time{}
	}
	return e.resolve(w).In(e.loc)
}

// Matches reports whether the wall time of t in the expression's zone
// (to the minute; seconds are ignored) matches every field, with cron's
// day-of-month/day-of-week rule.
func (e *Expr) Matches(t time.Time) bool {
	t = t.In(e.loc)
	return bit(e.minute, t.Minute()) && bit(e.hour, t.Hour()) && bit(e.month, int(t.Month())) &&
		e.dayMatches(t.Day(), t.Weekday())
}

// dayMatches applies cron's day rule (robfig's star-bit semantics): when
// both day fields are restricted, a day matches either; when one is "*" (or
// "?"), both must match, which leaves the other one deciding.
func (e *Expr) dayMatches(day int, weekday time.Weekday) bool {
	domMatch, dowMatch := bit(e.dom, day), bit(e.dow, int(weekday))
	if e.dom&starBit != 0 || e.dow&starBit != 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

func bit(set uint64, n int) bool { return set&(1<<uint(n)) != 0 }

// naiveMinute returns t's wall time, truncated to the minute, as a naive
// (UTC) time.
func naiveMinute(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// nextWall returns the first matching naive wall time strictly after w (a
// naive minute) and not after limit; ok is false if there is none.
func (e *Expr) nextWall(w, limit time.Time) (time.Time, bool) {
	w = w.Add(time.Minute)
	for !w.After(limit) {
		y, mo, d := w.Date()
		if !bit(e.month, int(mo)) {
			w = time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !e.dayMatches(d, w.Weekday()) {
			w = time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		h := nextBit(e.hour, w.Hour(), 23)
		if h < 0 {
			w = time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		m := 0
		if h == w.Hour() {
			m = w.Minute()
		}
		if m = nextBit(e.minute, m, 59); m < 0 {
			w = time.Date(y, mo, d, h+1, 0, 0, 0, time.UTC)
			continue
		}
		if w = time.Date(y, mo, d, h, m, 0, 0, time.UTC); w.After(limit) {
			break
		}
		return w, true
	}
	return time.Time{}, false
}

// prevWall returns the last matching naive wall time at or before w (a
// naive minute) and not before limit; ok is false if there is none.
func (e *Expr) prevWall(w, limit time.Time) (time.Time, bool) {
	for !w.Before(limit) {
		y, mo, d := w.Date()
		if !bit(e.month, int(mo)) {
			w = time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
			continue
		}
		if !e.dayMatches(d, w.Weekday()) {
			w = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
			continue
		}
		h := prevBit(e.hour, w.Hour())
		if h < 0 {
			w = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
			continue
		}
		m := 59
		if h == w.Hour() {
			m = w.Minute()
		}
		if m = prevBit(e.minute, m); m < 0 {
			w = time.Date(y, mo, d, h, 0, 0, 0, time.UTC).Add(-time.Minute)
			continue
		}
		if w = time.Date(y, mo, d, h, m, 0, 0, time.UTC); w.Before(limit) {
			break
		}
		return w, true
	}
	return time.Time{}, false
}

// nextBit returns the smallest n in [from, maxN] whose bit is set in set,
// or -1.
func nextBit(set uint64, from, maxN int) int {
	s := (set &^ starBit) >> uint(from)
	if s == 0 {
		return -1
	}
	if n := from + bits.TrailingZeros64(s); n <= maxN {
		return n
	}
	return -1
}

// prevBit returns the largest n in [0, from] whose bit is set in set, or
// -1.
func prevBit(set uint64, from int) int {
	s := (set &^ starBit) & (uint64(1)<<uint(from+1) - 1)
	if s == 0 {
		return -1
	}
	return 63 - bits.LeadingZeros64(s)
}

// resolve returns R(w), the instant at which the naive wall time w runs:
// the first instant at which the zone's clock shows w or later.
//
// Within one zone period (a constant offset off, from time.ZoneBounds) the
// clock shows c + off, so the period's first instant at or past w is
// max(period start, w - off), if that is still inside the period. Every
// instant more than maxOffset before w shows an earlier wall time, so
// walking the periods from there in order, the first period with such an
// instant holds R(w): the exact instant when w exists once, the first pass
// when it repeats, and the start of the period after the gap (the
// transition) when it does not exist. The walk is exact for any number of
// transitions.
func (e *Expr) resolve(w time.Time) time.Time {
	hi := w.Add(maxOffset)
	for t := w.Add(-maxOffset).In(e.loc); t.Before(hi); {
		_, off := t.Zone()
		_, end := t.ZoneBounds()
		c := w.Add(-time.Duration(off) * time.Second)
		if c.Before(t) {
			c = t
		}
		if end.IsZero() || c.Before(end) {
			return c
		}
		// ZoneBounds may report a period shorter than the real one (Go
		// computes zones beyond the last listed transition from the TZ rule
		// per year); it is never longer, and the walk resumes at its end.
		// A bound that does not advance cannot occur, but must not loop.
		if !end.After(t) {
			end = t.Add(time.Hour)
		}
		t = end.In(e.loc)
	}
	return hi
}
