package cronexpr_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // embedded zone data: the DST cases do not depend on the host's

	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error %v is not a *domain.ValidationError", err)
	}
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("error %v does not match ErrInvalidArgument", err)
	}
	var out []string
	for _, f := range verr.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestParseAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct{ expr, tz, norm string }{
		{"30 2 * * *", "Europe/Berlin", "30 2 * * *"},
		{"  */15   *  * *   * ", "UTC", "*/15 * * * *"},
		{"0 9 * * MON-FRI", "America/New_York", "0 9 * * mon-fri"},
		{"0 0 1 JAN,jul ?", "Asia/Kolkata", "0 0 1 jan,jul ?"},
		{"0 0 29 2 *", "UTC", "0 0 29 2 *"},
		{"@daily", "UTC", "@daily"},
		{"@Weekly", "UTC", "@weekly"},
		{"@yearly", "UTC", "@yearly"},
		{"@annually", "UTC", "@annually"},
		{"@monthly", "UTC", "@monthly"},
		{"@midnight", "UTC", "@midnight"},
		{"@hourly", "Australia/Lord_Howe", "@hourly"},
	}
	for _, tt := range tests {
		e, err := cronexpr.Parse(tt.expr, tt.tz)
		if err != nil {
			t.Errorf("Parse(%q, %q): %v", tt.expr, tt.tz, err)
			continue
		}
		if e.String() != tt.norm || e.Location().String() != tt.tz {
			t.Errorf("Parse(%q, %q) = %q in %s", tt.expr, tt.tz, e.String(), e.Location())
		}
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		expr, tz string
		want     []string
	}{
		{"", "UTC", []string{"cron"}},
		{"0 30 2 * * *", "UTC", []string{"cron"}}, // seconds
		{"* * * *", "UTC", []string{"cron"}},
		{"@every 5m", "UTC", []string{"cron"}},
		{"TZ=Europe/Berlin 0 2 * * *", "UTC", []string{"cron"}},
		{"CRON_TZ=UTC 0 2 * * *", "UTC", []string{"cron"}},
		{"@reboot", "UTC", []string{"cron"}},
		{"61 * * * *", "UTC", []string{"cron"}},
		{"0 0 31 2 *", "UTC", []string{"cron"}}, // never
		{"0 0 30 2 *", "Europe/Berlin", []string{"cron"}},
		{"0 0 31 4,6,9,11 *", "UTC", []string{"cron"}}, // never: no such day
		{"@every 1h30m", "Europe/Berlin", []string{"cron"}},
		{"cron_tz=Europe/Berlin 0 2 * * *", "UTC", []string{"cron"}},
		{"0 0 * * * *", "UTC", []string{"cron"}}, // six fields: seconds
		{"0 2 * * *", "local", []string{"timezone"}},
		{"0 2 * * *", "", []string{"timezone"}},
		{"0 2 * * *", "Local", []string{"timezone"}},
		{"0 2 * * *", "Mars/Olympus", []string{"timezone"}},
		{"0 2 * * *", " UTC", []string{"timezone"}},
		{"0 2 * * *", "../etc/passwd", []string{"timezone"}},
		{"bad", "nowhere", []string{"timezone", "cron"}},
	}
	for _, tt := range tests {
		_, err := cronexpr.Parse(tt.expr, tt.tz)
		if err == nil {
			t.Errorf("Parse(%q, %q) accepted it", tt.expr, tt.tz)
			continue
		}
		if got := fieldsOf(t, err); !slices.Equal(got, tt.want) {
			t.Errorf("Parse(%q, %q) fields = %v, want %v (%v)", tt.expr, tt.tz, got, tt.want, err)
		}
	}
}

func mustParse(t *testing.T, expr, tz string) *cronexpr.Expr {
	t.Helper()
	e, err := cronexpr.Parse(expr, tz)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Asia/Kolkata has no daylight saving time and a +05:30 offset.
func TestNextWithoutDST(t *testing.T) {
	t.Parallel()
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	e := mustParse(t, "30 2 * * *", "Asia/Kolkata")
	after := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) // 05:30 IST
	got := e.Next(after)
	if want := time.Date(2026, 11, 5, 2, 30, 0, 0, kolkata); !got.Equal(want) || got.Location().String() != "Asia/Kolkata" {
		t.Errorf("Next = %v, want %v", got, want)
	}
	if !e.Matches(got) || e.Matches(got.Add(time.Minute)) {
		t.Error("Matches is wrong")
	}
	// Strictly after.
	if again := e.Next(got); !again.Equal(got.Add(24 * time.Hour)) {
		t.Errorf("Next(occurrence) = %v", again)
	}
	five := e.NextN(after, 5)
	if len(five) != 5 || !five[4].Equal(got.Add(4*24*time.Hour)) {
		t.Errorf("NextN = %v", five)
	}
	if n := len(e.NextN(after, 1000)); n != cronexpr.MaxPreview {
		t.Errorf("NextN(1000) returned %d", n)
	}
	if n := len(e.NextN(after, -1)); n != 0 {
		t.Errorf("NextN(-1) returned %d", n)
	}
}

func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	t.Parallel()
	// Both restricted: the 13th or any Friday.
	either := mustParse(t, "0 0 13 * 5", "UTC")
	// Day of week "*": only the 13th.
	only := mustParse(t, "0 0 13 * *", "UTC")
	fri := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) // a Friday, the 6th
	thu13 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if !either.Matches(fri) || !either.Matches(thu13) || only.Matches(fri) || !only.Matches(thu13) {
		t.Error("day-of-month/day-of-week rule is wrong")
	}
	if got := either.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); !got.Equal(fri) {
		t.Errorf("Next = %v, want %v", got, fri)
	}
}

// TestSteppedStarDayField: a day field starting with "*" counts as "*" for
// the day rule even with a step, as in Vixie cron: "*/2" days that are
// Mondays, not odd days or Mondays.
func TestSteppedStarDayField(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 0 */2 * 1", "UTC")
	mon2 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) // a Monday, the 2nd (even)
	mon9 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) // a Monday, the 9th (odd)
	thu5 := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC) // a Thursday, the 5th (odd)
	if e.Matches(mon2) || !e.Matches(mon9) || e.Matches(thu5) {
		t.Error(`"0 0 */2 * 1" must match odd days that are Mondays only`)
	}
	if got := e.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); !got.Equal(mon9) {
		t.Errorf("Next = %v, want %v", got, mon9)
	}
	// The same with the day of week stepped: the 13th, if it is a Sunday,
	// Tuesday, Thursday or Saturday.
	e = mustParse(t, "0 0 13 * */2", "UTC")
	if fri13 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC); e.Matches(fri13) {
		t.Error(`"0 0 13 * */2" matches Friday the 13th`)
	}
	if sun13 := time.Date(2026, 12, 13, 0, 0, 0, 0, time.UTC); !e.Matches(sun13) {
		t.Error(`"0 0 13 * */2" does not match Sunday the 13th`)
	}
}

// TestSundayIsSeven: 7, and ranges through 7 or "sun", mean Sunday, as in
// crontab(5).
func TestSundayIsSeven(t *testing.T) {
	t.Parallel()
	week := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) // a Sunday
	tests := []struct {
		expr string
		days string // the matching days of the week from Sunday, as 0/1
	}{
		{"0 0 * * 7", "1000000"},
		{"0 0 * * 0", "1000000"},
		{"0 0 * * 5-7", "1000011"},
		{"0 0 * * fri-sun", "1000011"},
		{"0 0 * * mon-sun", "1111111"},
		{"0 0 * * 0-7", "1111111"},
		{"0 0 * * 1-7/2", "1101010"},
		{"0 0 * * 2-7/2", "0010101"},
		{"0 0 * * 7-7", "1000000"},
		{"0 0 * * 1,7", "1100000"},
		{"0 0 * * sat-sun", "1000001"},
	}
	for _, tt := range tests {
		e, err := cronexpr.Parse(tt.expr, "UTC")
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.expr, err)
			continue
		}
		if e.String() != tt.expr {
			t.Errorf("Parse(%q) normalized to %q", tt.expr, e.String())
		}
		var got strings.Builder
		for i := range 7 {
			if e.Matches(week.AddDate(0, 0, i)) {
				got.WriteByte('1')
			} else {
				got.WriteByte('0')
			}
		}
		if got.String() != tt.days {
			t.Errorf("%q matches %s, want %s", tt.expr, got.String(), tt.days)
		}
	}
	for _, bad := range []string{"0 0 * * 8", "0 0 * * 7/2", "0 0 * * 1-7/0", "0 0 * * 3-1"} {
		if _, err := cronexpr.Parse(bad, "UTC"); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "0 * * * *", "UTC")
	after := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	until := after.Add(5*time.Hour + 30*time.Minute)
	last, n := e.Latest(after, until, 1000)
	if n != 5 || !last.Equal(after.Add(5*time.Hour)) {
		t.Errorf("Latest = (%v, %d), want 05:00 and 5", last, n)
	}
	// until is inclusive, after exclusive.
	if last, n := e.Latest(after, after.Add(time.Hour), 1000); n != 1 || !last.Equal(after.Add(time.Hour)) {
		t.Errorf("Latest(inclusive) = (%v, %d)", last, n)
	}
	if last, n := e.Latest(after, after.Add(59*time.Minute), 1000); n != 0 || !last.IsZero() {
		t.Errorf("Latest(none) = (%v, %d)", last, n)
	}
	// The count is capped; the latest occurrence is still exact.
	last, n = e.Latest(after, after.Add(30*24*time.Hour+10*time.Minute), 3)
	if n != 3 || !last.Equal(after.Add(30*24*time.Hour)) {
		t.Errorf("Latest(capped) = (%v, %d)", last, n)
	}
	every := mustParse(t, "*/5 * * * *", "UTC")
	last, n = every.Latest(after, after.Add(7*24*time.Hour+3*time.Minute), 10)
	if n != 10 || !last.Equal(after.Add(7*24*time.Hour)) {
		t.Errorf("Latest(*/5, capped) = (%v, %d)", last, n)
	}
}

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func instants(ts []time.Time) []time.Time {
	out := make([]time.Time, len(ts))
	for i, t := range ts {
		out[i] = t.UTC()
	}
	return out
}

// TestNextDST is the daylight saving time table: occurrences are local wall
// times; a wall time in a spring-forward gap runs at the transition (several
// of them collapse into one run), a repeated wall time runs once, at its
// first instant. Expected instants are in UTC.
func TestNextDST(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		expr, tz string
		after    time.Time
		want     []time.Time
	}{
		// Europe/Berlin: 2026-03-29 02:00 CET -> 03:00 CEST at 01:00 UTC;
		// 2026-10-25 03:00 CEST -> 02:00 CET at 01:00 UTC.
		{"berlin spring 02:30 runs at 03:00", "30 2 * * *", "Europe/Berlin", utc(2026, 3, 28, 12, 0),
			[]time.Time{utc(2026, 3, 29, 1, 0), utc(2026, 3, 30, 0, 30)}},
		{"berlin spring */15 collapses in the gap", "*/15 * * * *", "Europe/Berlin", utc(2026, 3, 29, 0, 30),
			[]time.Time{utc(2026, 3, 29, 0, 45), utc(2026, 3, 29, 1, 0), utc(2026, 3, 29, 1, 15)}},
		{"berlin spring gap and 03:00 run once", "0 2,3 * * *", "Europe/Berlin", utc(2026, 3, 29, 0, 0),
			[]time.Time{utc(2026, 3, 29, 1, 0), utc(2026, 3, 30, 0, 0), utc(2026, 3, 30, 1, 0)}},
		{"berlin fall 02:30 runs once, first pass", "30 2 * * *", "Europe/Berlin", utc(2026, 10, 24, 12, 0),
			[]time.Time{utc(2026, 10, 25, 0, 30), utc(2026, 10, 26, 1, 30)}},
		{"berlin fall */15 only in the first pass", "*/15 * * * *", "Europe/Berlin", utc(2026, 10, 24, 23, 40),
			[]time.Time{
				utc(2026, 10, 24, 23, 45), utc(2026, 10, 25, 0, 0), utc(2026, 10, 25, 0, 15),
				utc(2026, 10, 25, 0, 30), utc(2026, 10, 25, 0, 45), utc(2026, 10, 25, 2, 0),
			}},
		{"berlin fall from inside the second pass", "*/15 * * * *", "Europe/Berlin", utc(2026, 10, 25, 1, 20),
			[]time.Time{utc(2026, 10, 25, 2, 0)}},
		{"berlin fall 02:30 from the second pass", "30 2 * * *", "Europe/Berlin", utc(2026, 10, 25, 1, 10),
			[]time.Time{utc(2026, 10, 26, 1, 30)}},

		// America/New_York: 2026-03-08 02:00 EST -> 03:00 EDT at 07:00 UTC;
		// 2026-11-01 02:00 EDT -> 01:00 EST at 06:00 UTC.
		{"new york spring 02:30 runs at 03:00", "30 2 * * *", "America/New_York", utc(2026, 3, 7, 12, 0),
			[]time.Time{utc(2026, 3, 8, 7, 0), utc(2026, 3, 9, 6, 30)}},
		{"new york spring */30 collapses", "*/30 * * * *", "America/New_York", utc(2026, 3, 8, 6, 15),
			[]time.Time{utc(2026, 3, 8, 6, 30), utc(2026, 3, 8, 7, 0), utc(2026, 3, 8, 7, 30)}},
		{"new york fall 01:30 runs once", "30 1 * * *", "America/New_York", utc(2026, 10, 31, 12, 0),
			[]time.Time{utc(2026, 11, 1, 5, 30), utc(2026, 11, 2, 6, 30)}},
		{"new york fall hourly skips the repeated hour", "0 * * * *", "America/New_York", utc(2026, 11, 1, 4, 30),
			[]time.Time{utc(2026, 11, 1, 5, 0), utc(2026, 11, 1, 7, 0), utc(2026, 11, 1, 8, 0)}},

		// Australia/Lord_Howe shifts by 30 minutes: 2026-10-04 02:00 +10:30
		// -> 02:30 +11 at 15:30 UTC (Oct 3); 2026-04-05 02:00 +11 -> 01:30
		// +10:30 at 15:00 UTC (Apr 4).
		{"lord howe spring 02:15 runs at 02:30", "15 2 * * *", "Australia/Lord_Howe", utc(2026, 10, 3, 0, 0),
			[]time.Time{utc(2026, 10, 3, 15, 30), utc(2026, 10, 4, 15, 15)}},
		{"lord howe fall 01:45 runs once", "45 1 * * *", "Australia/Lord_Howe", utc(2026, 4, 4, 0, 0),
			[]time.Time{utc(2026, 4, 4, 14, 45), utc(2026, 4, 5, 15, 15)}},
		{"lord howe fall */10 in the first pass only", "*/10 1,2 * * *", "Australia/Lord_Howe", utc(2026, 4, 4, 14, 25),
			[]time.Time{
				utc(2026, 4, 4, 14, 30), utc(2026, 4, 4, 14, 40), utc(2026, 4, 4, 14, 50),
				utc(2026, 4, 4, 15, 30), utc(2026, 4, 4, 15, 40),
			}},

		// America/Sao_Paulo 2018: midnight of Nov 4 did not exist (00:00 -03
		// -> 01:00 -02 at 03:00 UTC); 2019-02-16 23:00 was repeated
		// (00:00 -02 -> 23:00 -03 at 02:00 UTC on Feb 17).
		{"sao paulo 2018 midnight runs at 01:00", "0 0 * * *", "America/Sao_Paulo", utc(2018, 11, 3, 12, 0),
			[]time.Time{utc(2018, 11, 4, 3, 0), utc(2018, 11, 5, 2, 0)}},
		{"sao paulo 2018 @daily is midnight", "@daily", "America/Sao_Paulo", utc(2018, 11, 3, 12, 0),
			[]time.Time{utc(2018, 11, 4, 3, 0)}},
		{"sao paulo 2019 23:00 runs once", "0 23 * * *", "America/Sao_Paulo", utc(2019, 2, 16, 12, 0),
			[]time.Time{utc(2019, 2, 17, 1, 0), utc(2019, 2, 18, 2, 0)}},

		// Pacific/Apia skipped 2011-12-30: 2011-12-29 24:00 -10 became
		// 2011-12-31 00:00 +14 at 10:00 UTC on Dec 30.
		{"apia 2011 skipped day runs at the transition", "0 12 * * *", "Pacific/Apia", utc(2011, 12, 29, 23, 0),
			[]time.Time{utc(2011, 12, 30, 10, 0), utc(2011, 12, 30, 22, 0)}},
		{"apia 2011 yearly on the skipped day", "0 0 30 12 *", "Pacific/Apia", utc(2011, 6, 1, 0, 0),
			[]time.Time{utc(2011, 12, 30, 10, 0), utc(2012, 12, 29, 10, 0)}}, // +14 (DST) in 2012 too

		// Asia/Kolkata: no DST, +05:30.
		{"kolkata", "30 2 * * *", "Asia/Kolkata", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2026, 11, 4, 21, 0), utc(2026, 11, 5, 21, 0)}},
		{"kolkata hourly at :00 is :30 utc", "@hourly", "Asia/Kolkata", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2026, 11, 4, 0, 30), utc(2026, 11, 4, 1, 30)}},

		// Descriptors.
		{"weekly is sunday midnight", "@weekly", "UTC", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2026, 11, 8, 0, 0), utc(2026, 11, 15, 0, 0)}},
		{"monthly", "@monthly", "Europe/Berlin", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2026, 11, 30, 23, 0), utc(2026, 12, 31, 23, 0)}},
		{"yearly", "@yearly", "UTC", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2027, 1, 1, 0, 0), utc(2028, 1, 1, 0, 0)}},
		{"leap day", "0 0 29 2 *", "UTC", utc(2026, 11, 4, 0, 0),
			[]time.Time{utc(2028, 2, 29, 0, 0), utc(2032, 2, 29, 0, 0)}},
		{"leap day beyond the horizon", "0 0 29 2 *", "UTC", utc(2097, 3, 1, 0, 0), nil},
	}
	for _, tt := range tests {
		e := mustParse(t, tt.expr, tt.tz)
		got := instants(e.NextN(tt.after, len(tt.want)))
		if !slices.EqualFunc(got, tt.want, time.Time.Equal) {
			t.Errorf("%s: NextN(%q in %s, after %v) = %v, want %v", tt.name, tt.expr, tt.tz, tt.after, got, tt.want)
		}
		for _, o := range e.NextN(tt.after, len(tt.want)) {
			if o.Location() != e.Location() {
				t.Errorf("%s: occurrence %v is not in the expression's zone", tt.name, o)
			}
		}
	}
}

// TestNextExactlyOnce walks every occurrence of "* * * * *" across both
// Berlin transitions: each existing wall time runs once, the gap runs once
// at the transition, and occurrences strictly increase.
func TestNextExactlyOnce(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "* * * * *", "Europe/Berlin")
	for _, tt := range []struct {
		from, until time.Time
		want        int
	}{
		// 00:00-04:00 UTC on the spring day: 4h of instants, minus the gap's
		// 60 wall times plus their one run at 03:00 CEST (which is also
		// 03:00's run).
		{utc(2026, 3, 29, 0, 0), utc(2026, 3, 29, 4, 0), 240},
		// The fall day: the repeated hour runs once, so one hour fewer.
		{utc(2026, 10, 25, 0, 0), utc(2026, 10, 25, 4, 0), 180},
	} {
		n, prev := 0, tt.from
		seen := map[time.Time]bool{}
		for {
			next := e.Next(prev)
			if next.After(tt.until) {
				break
			}
			if !next.After(prev) {
				t.Fatalf("Next(%v) = %v is not after it", prev, next)
			}
			wall := time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), next.Minute(), 0, 0, time.UTC)
			if seen[wall] {
				t.Errorf("wall time %v ran twice", wall)
			}
			seen[wall] = true
			n, prev = n+1, next
		}
		if n != tt.want {
			t.Errorf("occurrences in (%v, %v] = %d, want %d", tt.from, tt.until, n, tt.want)
		}
	}
}

func TestLatestAcrossDST(t *testing.T) {
	t.Parallel()
	e := mustParse(t, "*/15 * * * *", "Europe/Berlin")
	tests := []struct {
		name         string
		after, until time.Time
		max          int
		wantLast     time.Time
		wantCount    int
	}{
		// 01:15, 01:30, 01:45 CET, then the gap's one run at 03:00 CEST,
		// 03:15 and 03:30.
		{"spring", utc(2026, 3, 29, 0, 0), utc(2026, 3, 29, 1, 30), 1000, utc(2026, 3, 29, 1, 30), 6},
		// 02:15, 02:30, 02:45 CEST, then 03:00 CET; the second pass of the
		// repeated hour does not run.
		{"fall", utc(2026, 10, 25, 0, 0), utc(2026, 10, 25, 2, 0), 1000, utc(2026, 10, 25, 2, 0), 4},
		// Capped inside the second pass: the latest occurrence is the first
		// pass's 02:45 CEST.
		{"fall capped", utc(2026, 10, 24, 0, 0), utc(2026, 10, 25, 1, 40), 3, utc(2026, 10, 25, 0, 45), 3},
		// Capped just after the gap: its run at 03:00 CEST.
		{"spring capped", utc(2026, 3, 28, 0, 0), utc(2026, 3, 29, 1, 5), 5, utc(2026, 3, 29, 1, 0), 5},
	}
	for _, tt := range tests {
		last, n := e.Latest(tt.after, tt.until, tt.max)
		if n != tt.wantCount || !last.Equal(tt.wantLast) {
			t.Errorf("%s: Latest = (%v, %d), want (%v, %d)", tt.name, last.UTC(), n, tt.wantLast, tt.wantCount)
		}
	}
}
