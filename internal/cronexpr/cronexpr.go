package cronexpr

import (
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits.
const (
	// MaxPreview bounds NextN.
	MaxPreview = 20
	// MaxLen bounds an expression, in bytes.
	MaxLen = 256
	// MaxTimezoneLen bounds a time zone name, in bytes.
	MaxTimezoneLen = 64
	// horizonYears is how far Next looks for an occurrence.
	horizonYears = 5
)

// starBit marks a field written as "*" (or "?") in robfig's bit sets.
const starBit = 1 << 63

// Expr is a parsed cron expression in a time zone. It is immutable and safe
// for concurrent use.
type Expr struct {
	// Bit sets of the matching values (bit n set: value n matches), as
	// robfig's parser builds them; starBit is set in a field written "*".
	minute, hour, dom, month, dow uint64
	loc                           *time.Location
	// src is the normalized expression.
	src string
	// spec is robfig's schedule, used by the provisional occurrence
	// functions.
	spec *cron.SpecSchedule
}

// parser accepts the five standard fields and the descriptors.
func parser() cron.Parser {
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
}

// Normalize returns expr as Parse stores it: lower case, trimmed, with runs
// of white space collapsed to one space.
func Normalize(expr string) string {
	return strings.ToLower(strings.Join(strings.Fields(expr), " "))
}

// Parse parses a cron expression in the IANA time zone tz (see the package
// documentation for the syntax). Errors are a *domain.ValidationError with
// the field "cron" and/or "timezone".
func Parse(expr, tz string) (*Expr, error) {
	var v domain.Validation
	loc, msg := loadLocation(tz)
	if msg != "" {
		v.Add("timezone", msg)
	}
	src := Normalize(expr)
	spec, msg := parseSpec(src, len(expr))
	if msg != "" {
		v.Add("cron", msg)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	spec.Location = loc
	e := &Expr{
		minute: spec.Minute, hour: spec.Hour, dom: spec.Dom, month: spec.Month, dow: spec.Dow,
		loc: loc, src: src, spec: spec,
	}
	// A fixed reference keeps Parse pure; within five years of it every
	// possible date (February 29 included) occurs.
	if spec.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, loc)).IsZero() {
		v.Add("cron", "never matches a date (e.g. February 30)")
		return nil, v.Err()
	}
	return e, nil
}

// loadLocation resolves a time zone name; msg says what is wrong.
func loadLocation(tz string) (loc *time.Location, msg string) {
	switch {
	case tz == "":
		return nil, "is required"
	case tz == "Local" || len(tz) > MaxTimezoneLen || strings.TrimSpace(tz) != tz:
		return nil, `must be an IANA time zone name such as "Europe/Berlin" or "UTC"`
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, "is not a known time zone"
	}
	return loc, ""
}

// parseSpec parses a normalized expression; msg says what is wrong.
func parseSpec(src string, rawLen int) (spec *cron.SpecSchedule, msg string) {
	switch {
	case src == "":
		return nil, "is required"
	case rawLen > MaxLen:
		return nil, "is too long"
	case strings.HasPrefix(src, "tz=") || strings.HasPrefix(src, "cron_tz="):
		return nil, "must not include a time zone; use the time zone field"
	case strings.HasPrefix(src, "@every"):
		return nil, "@every is not supported; use a cron expression such as */15 * * * *"
	case !strings.HasPrefix(src, "@"):
		switch n := len(strings.Fields(src)); {
		case n == 6:
			return nil, "has 6 fields: seconds are not supported; use 5 fields (minute hour day-of-month month day-of-week)"
		case n != 5:
			return nil, "must have 5 fields (minute hour day-of-month month day-of-week) or be a descriptor such as @daily"
		}
	}
	sched, err := parser().Parse(src)
	if err != nil {
		return nil, "is not a valid cron expression (" + err.Error() + ")"
	}
	spec, ok := sched.(*cron.SpecSchedule)
	if !ok {
		return nil, "is not a valid cron expression"
	}
	return spec, ""
}

// Next returns the first occurrence strictly after after, in the
// expression's zone, or the zero time if there is none within five years.
//
// Provisional (contracts step): robfig's iteration.
func (e *Expr) Next(after time.Time) time.Time {
	t := e.spec.Next(after)
	if t.IsZero() || t.Year() > after.Year()+horizonYears {
		return time.Time{}
	}
	return t.In(e.loc)
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
	// More than maxCount: find the latest occurrence <= until by bisection
	// over the start of the search (Next is monotonic).
	if next := e.Next(last); next.IsZero() || next.After(until) {
		return last, count
	}
	lo, hi := last, until // Next(lo) <= until < Next(hi)
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if next := e.Next(mid); !next.IsZero() && !next.After(until) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return e.Next(lo), count
}

// Matches reports whether the wall time of t in the expression's zone
// (to the minute; seconds are ignored) matches every field, with cron's
// day-of-month/day-of-week rule.
func (e *Expr) Matches(t time.Time) bool {
	t = t.In(e.loc)
	if !bit(e.minute, t.Minute()) || !bit(e.hour, t.Hour()) || !bit(e.month, int(t.Month())) {
		return false
	}
	domMatch, dowMatch := bit(e.dom, t.Day()), bit(e.dow, int(t.Weekday()))
	if e.dom&starBit != 0 || e.dow&starBit != 0 {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

func bit(set uint64, n int) bool { return set&(1<<uint(n)) != 0 }

// Location returns the expression's time zone.
func (e *Expr) Location() *time.Location { return e.loc }

// String returns the normalized expression (see Normalize).
func (e *Expr) String() string { return e.src }
