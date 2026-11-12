package postgres

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestInfoRoundTrip(t *testing.T) {
	t.Parallel()
	in := workers.Info{
		Version: "v2.0.0", OS: "linux", Arch: "arm64", Hostname: "h",
		Tools:             []workers.Tool{{Name: "rclone", Version: "1.70"}, {Name: "restic", Version: "0.18"}},
		JobKinds:          []string{"rclone_transfer", "restic_backup"},
		MaxConcurrentJobs: 3,
		LocalRoots:        []string{"/data", "/srv"},
		ReportedAt:        time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.UTC),
	}
	doc, err := encodeInfo(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeInfo(doc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != in.Version || out.OS != in.OS || out.Arch != in.Arch || out.Hostname != in.Hostname ||
		!slices.Equal(out.Tools, in.Tools) || !slices.Equal(out.JobKinds, in.JobKinds) ||
		out.MaxConcurrentJobs != in.MaxConcurrentJobs || !slices.Equal(out.LocalRoots, in.LocalRoots) ||
		!out.ReportedAt.Equal(in.ReportedAt) {
		t.Errorf("round trip:\n got %+v\nwant %+v", out, in)
	}
}

func TestInfoZeroAndEmpty(t *testing.T) {
	t.Parallel()
	doc, err := encodeInfo(workers.Info{})
	if err != nil {
		t.Fatal(err)
	}
	if string(doc) != "{}" {
		t.Errorf("zero Info encodes as %s, want {}", doc)
	}
	for _, stored := range [][]byte{nil, []byte("{}"), []byte(`{"unknown_future_field":1}`)} {
		out, err := decodeInfo(stored)
		if err != nil || !out.ReportedAt.IsZero() || out.Version != "" || len(out.Tools) != 0 {
			t.Errorf("decodeInfo(%s) = (%+v, %v)", stored, out, err)
		}
	}
	if _, err := decodeInfo([]byte("not json")); err == nil {
		t.Error("decodeInfo(garbage) succeeded")
	}
}

func TestTimeConversions(t *testing.T) {
	t.Parallel()
	if got := pgOptTime(time.Time{}); got.Valid {
		t.Error("zero time must be NULL in optional columns")
	}
	if got := pgTime(time.Time{}); !got.Valid {
		t.Error("required columns are never NULL")
	}
	if !timeFrom(pgtype.Timestamptz{}).IsZero() || !timeFrom(pgTime(time.Time{})).IsZero() {
		t.Error("NULL and the stored zero time must read back as the zero time")
	}
	in := time.Date(2026, 11, 4, 13, 0, 0, 999999999, time.FixedZone("x", 7200))
	got := timeFrom(pgTime(in))
	if want := in.Truncate(time.Microsecond); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("round trip = %v, want %v in UTC", got, want)
	}
}

func TestIDConversions(t *testing.T) {
	t.Parallel()
	id := domain.NewJobID()
	var back domain.JobID
	if err := decodeID(&back, pgID(id)); err != nil || back != id {
		t.Errorf("round trip = (%v, %v), want %v", back, err, id)
	}
	if pgOptID(domain.JobID{}).Valid {
		t.Error("zero ID must be NULL in optional columns")
	}
	if !pgID(domain.JobID{}).Valid {
		t.Error("required IDs are never NULL")
	}
	var zero domain.JobID
	if err := decodeID(&zero, pgtype.UUID{}); err != nil || !zero.IsZero() {
		t.Errorf("NULL decoded to (%v, %v), want the zero ID", zero, err)
	}
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain":      "plain",
		"nul\x00in":  "nul�in",
		"bad\xffutf": "bad�utf",
		"ünïcode ✓":  "ünïcode ✓",
	} {
		if got := cleanText(in); got != want {
			t.Errorf("cleanText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimitArg(t *testing.T) {
	t.Parallel()
	if limitArg(-1) != nil {
		t.Error("negative limit means no limit (NULL)")
	}
	if p := limitArg(0); p == nil || *p != 0 {
		t.Error("limit 0 means no rows")
	}
	if p := limitArg(7); p == nil || *p != 7 {
		t.Error("limit 7")
	}
}

func TestEmbeddedMigrationsAreSequential(t *testing.T) {
	t.Parallel()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i, e := range entries {
		want := fmt.Sprintf("%05d_", i+1)
		if !strings.HasPrefix(e.Name(), want) {
			t.Errorf("migration %d is %s, want prefix %s (versions must be gapless)", i, e.Name(), want)
		}
	}
}

func TestLikeEscape(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":             "",
		"photos":       "photos",
		"100%":         `100\%`,
		"a_b":          `a\_b`,
		`c:\data`:      `c:\\data`,
		`\%_`:          `\\\%\_`,
		"ünïcode 50 %": `ünïcode 50 \%`,
	} {
		if got := likeEscape(in); got != want {
			t.Errorf("likeEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHookRunsRoundTrip(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	runs := []jobs.HookRun{
		{Name: "stop-app", Phase: domain.HookBefore, Status: domain.HookSucceeded, StartedAt: at, Duration: 1500 * time.Millisecond, Output: "stopping “app”\nline 2"},
		{Name: "start-app", Phase: domain.HookAfter, Status: domain.HookFailed, ExitCode: -1, Message: "could not be started", OutputTruncated: true},
	}
	b, err := encodeHookRuns(runs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeHookRuns(b)
	if err != nil || len(got) != 2 {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	// The instant is kept to the nanosecond, in UTC.
	if !got[0].StartedAt.Equal(at) || got[0].StartedAt.Location() != time.UTC || !got[1].StartedAt.IsZero() {
		t.Errorf("started at = %v and %v", got[0].StartedAt, got[1].StartedAt)
	}
	got[0].StartedAt, runs[0].StartedAt = time.Time{}, time.Time{}
	if !slices.Equal(got, runs) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, runs)
	}

	// No runs are an empty array (the column is NOT NULL), which reads back
	// as nil, as does a row without the column's value.
	for _, none := range [][]jobs.HookRun{nil, {}} {
		if b, err := encodeHookRuns(none); err != nil || string(b) != "[]" {
			t.Errorf("encode(%v) = %s, %v", none, b, err)
		}
	}
	for _, empty := range [][]byte{nil, []byte("[]"), []byte("null")} {
		if got, err := decodeHookRuns(empty); err != nil || got != nil {
			t.Errorf("decode(%q) = %+v, %v", empty, got, err)
		}
	}
	if _, err := decodeHookRuns([]byte(`{"not":"an array"}`)); err == nil {
		t.Error("decode accepted an object")
	}

	// What jsonb cannot hold is replaced, not refused: a hook prints what it
	// likes.
	b, err = encodeHookRuns([]jobs.HookRun{{Name: "x", Output: "a\x00b\xffc", Message: "m\x00"}})
	if err != nil || strings.Contains(string(b), `\u0000`) {
		t.Fatalf("encode with NUL and invalid UTF-8 = %s, %v", b, err)
	}
	if got, err := decodeHookRuns(b); err != nil || got[0].Output != "a\uFFFDb\uFFFDc" || got[0].Message != "m\uFFFD" {
		t.Errorf("decode = %+v, %v", got, err)
	}
}
