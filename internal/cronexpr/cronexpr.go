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
	// horizonYears is how far Next looks for an occurrence (and Latest
	// back for the latest one).
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
	e := &Expr{
		minute: spec.Minute, hour: spec.Hour, dom: spec.Dom, month: spec.Month, dow: spec.Dow,
		loc: loc, src: src,
	}
	// A fixed reference keeps Parse pure; within five years of it every
	// possible date (February 29 included) occurs, so an expression without
	// a matching wall time there never matches.
	ref := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := e.nextWall(ref.Add(-time.Minute), ref.AddDate(horizonYears, 0, 0)); !ok {
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

// Location returns the expression's time zone.
func (e *Expr) Location() *time.Location { return e.loc }

// String returns the normalized expression (see Normalize).
func (e *Expr) String() string { return e.src }
