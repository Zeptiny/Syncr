package cronexpr_test

import (
	"errors"
	"slices"
	"testing"
	"time"

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

// Zones without daylight saving time behave the same under the provisional
// and the final occurrence functions.
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
