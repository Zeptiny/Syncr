package cronexpr

import (
	"slices"
	"strconv"
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
	// Parse also sets it in a day field that starts with "*" or "?" (such
	// as "*/2"), whose star robfig clears, as Vixie cron does (dayMatches).
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
	// Vixie cron (and cronie) treat a day field whose text starts with "*"
	// as unrestricted for the day rule, steps included ("*/2"); robfig
	// clears its star bit for a step, which would OR the two day fields.
	if f := strings.Fields(src); len(f) == 5 {
		if starred(f[2]) {
			e.dom |= starBit
		}
		if starred(f[4]) {
			e.dow |= starBit
		}
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
	parsed := src
	if f := strings.Fields(src); len(f) == 5 {
		f[4] = sundaySeven(f[4])
		parsed = strings.Join(f, " ")
	}
	sched, err := parser().Parse(parsed)
	if err != nil {
		return nil, "is not a valid cron expression (" + err.Error() + ")"
	}
	spec, ok := sched.(*cron.SpecSchedule)
	if !ok {
		return nil, "is not a valid cron expression"
	}
	return spec, ""
}

// starred reports whether a day field counts as "*" for the day rule: its
// text starts with "*" or "?", as Vixie cron decides it.
func starred(field string) bool {
	return strings.HasPrefix(field, "*") || strings.HasPrefix(field, "?")
}

// weekday reads a day-of-week number (0 to 7) or name (as robfig accepts
// them: "sun" to "sat").
func weekday(s string) (int, bool) {
	if n := slices.Index([]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, s); n >= 0 {
		return n, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 7 {
		return 0, false
	}
	return n, true
}

// sundaySeven rewrites a day-of-week field so that 7 means Sunday, as in
// crontab(5), for robfig, which knows only 0 to 6: "7" becomes "0", and a
// range that ends in 7 or "sun" ("5-7", "mon-sun", "1-7/2") ends at 6 with
// 0 added when the range (with its step) reaches 7. Anything else is left
// to robfig's parser, which rejects what is invalid.
func sundaySeven(field string) string {
	items := strings.Split(field, ",")
	out := make([]string, 0, len(items)+1)
	for _, item := range items {
		rng, step, hasStep := strings.Cut(item, "/")
		lo, hi, isRange := strings.Cut(rng, "-")
		a, okA := weekday(lo)
		if !okA || !isRange {
			if okA && a == 7 && !hasStep {
				item = "0"
			}
			out = append(out, item)
			continue
		}
		b, okB := weekday(hi)
		if okB && b == 0 && a > 0 {
			b = 7 // "mon-sun", "6-0": through Sunday
		}
		s := 1
		if hasStep {
			n, err := strconv.Atoi(step)
			if err != nil || n <= 0 {
				okB = false
			}
			s = n
		}
		if !okB || b != 7 || a > b {
			out = append(out, item)
			continue
		}
		if a < 7 {
			r := strconv.Itoa(a) + "-6"
			if hasStep {
				r += "/" + step
			}
			out = append(out, r)
		}
		if (7-a)%s == 0 {
			out = append(out, "0")
		}
	}
	return strings.Join(out, ",")
}

// Location returns the expression's time zone.
func (e *Expr) Location() *time.Location { return e.loc }

// String returns the normalized expression (see Normalize).
func (e *Expr) String() string { return e.src }
