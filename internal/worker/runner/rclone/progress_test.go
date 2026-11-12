package rclone

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// fixtureLines reads a testdata file captured from rclone v1.75.2 (local
// paths rewritten to /data/...).
func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// feed passes lines to a new tracker and returns it with the emitted
// progress.
func feed(lines ...string) (*tracker, []runner.Progress) {
	var emitted []runner.Progress
	t := newTracker(func(p runner.Progress) { emitted = append(emitted, p) }, nil)
	for _, l := range lines {
		t.line(proc.Line{Data: []byte(l)})
	}
	return t, emitted
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func TestTrackerCopyFixture(t *testing.T) {
	t.Parallel()
	tr, got := feed(fixtureLines(t, "copy_progress.jsonl")...)
	want := []runner.Progress{
		{Bytes: 1082342, TotalBytes: 2800006, Files: 2, TotalFiles: 3, Elapsed: secs(1.00134646)},
		{Bytes: 2130918, TotalBytes: 2800006, Files: 2, TotalFiles: 3, Elapsed: secs(2.000669151)},
		{Bytes: 2800006, TotalBytes: 2800006, Files: 3, TotalFiles: 3, Elapsed: secs(2.668178729)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("emitted\n%+v\nwant\n%+v", got, want)
	}
	if tr.progress != want[2] || len(tr.diagnostics()) != 0 {
		t.Errorf("final = %+v, diagnostics %q", tr.progress, tr.diagnostics())
	}
}

func TestTrackerSyncFixture(t *testing.T) {
	t.Parallel()
	_, got := feed(fixtureLines(t, "sync_renames_deletes.jsonl")...)
	want := []runner.Progress{{Checks: 7, Deletes: 2, Renames: 1, Elapsed: secs(0.000871769)}}
	if !slices.Equal(got, want) {
		t.Errorf("emitted %+v, want %+v", got, want)
	}
}

func TestTrackerMissingSourceFixture(t *testing.T) {
	t.Parallel()
	tr, got := feed(fixtureLines(t, "missing_source.jsonl")...)
	if len(got) != 1 || got[0].Errors != 1 {
		t.Errorf("emitted %+v, want one sample with 1 error", got)
	}
	// Three attempts log the same root error; it is kept once, and only the
	// last three distinct messages are kept.
	want := []string{
		"Attempt 2/3 failed with 1 errors and: directory not found",
		"Local file system at /data/missing: error reading source root directory: directory not found",
		"Attempt 3/3 failed with 1 errors and: directory not found",
	}
	if d := tr.diagnostics(); !slices.Equal(d, want) {
		t.Errorf("diagnostics =\n%q\nwant\n%q", d, want)
	}
}

func TestTrackerStartupErrorFixture(t *testing.T) {
	t.Parallel()
	tr, got := feed(fixtureLines(t, "bad_filter.txt")...)
	if len(got) != 0 {
		t.Errorf("emitted %+v", got)
	}
	d := tr.diagnostics()
	want := `Failed to initialise global options: failed to reload "filter" options: bad glob pattern` //nolint:misspell // rclone's own spelling
	if len(d) != 1 || !strings.HasPrefix(d[0], want) {
		t.Errorf("diagnostics = %q", d)
	}
}

// TestTrackerIgnoresUnknownKeys guards against the bug that broke Syncr v1:
// new keys, changed types of unused keys, nested objects and arrays from a
// newer rclone must not break progress parsing.
func TestTrackerIgnoresUnknownKeys(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"time":"2030-01-01T00:00:00Z","level":"notice","msg":"stats","newTopLevel":{"a":[1,2,{"b":null}]},"source":42,` +
			`"stats":{"bytes":100,"totalBytes":1000,"transfers":1,"totalTransfers":10,"checks":2,"deletes":0,"renames":0,"errors":0,` +
			`"elapsedTime":1.5,"eta":"soon","speed":{"avg":1},"transferring":"not-a-list","futureCounter":12345678901234567890,` +
			`"nested":{"deep":{"deeper":[true,false]}}}}`,
		// Integers written as floats or exponents still count.
		`{"level":"notice","stats":{"bytes":2.5e2,"totalBytes":1000.0,"transfers":2,"totalTransfers":10,"elapsedTime":2}}`,
		// A known counter with an unexpected type is treated as zero, and
		// counters never go backwards.
		`{"level":"notice","stats":{"bytes":"300","totalBytes":null,"transfers":3,"totalTransfers":10,"elapsedTime":3}}`,
		`{"level":"info","msg":"no stats here"}`,
		`{"level":"notice","stats":null}`,
		`not json at all`,
		`[1,2,3]`,
		`{"level":"notice","stats":{"bytes":`, // cut off
		``,
	}
	_, got := feed(lines...)
	want := []runner.Progress{
		{Bytes: 100, TotalBytes: 1000, Files: 1, TotalFiles: 10, Checks: 2, Elapsed: secs(1.5)},
		{Bytes: 250, TotalBytes: 1000, Files: 2, TotalFiles: 10, Checks: 2, Elapsed: secs(2)},
		{Bytes: 250, TotalBytes: 0, Files: 3, TotalFiles: 10, Checks: 2, Elapsed: secs(3)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("emitted\n%+v\nwant\n%+v", got, want)
	}
}

func TestTrackerCountersNeverDecrease(t *testing.T) {
	t.Parallel()
	// rclone resets its error count before retrying the whole operation.
	_, got := feed(
		`{"level":"notice","stats":{"bytes":10,"errors":3,"checks":5,"elapsedTime":1}}`,
		`{"level":"notice","stats":{"bytes":5,"errors":0,"checks":1,"elapsedTime":0.5}}`,
		`{"level":"notice","stats":{"bytes":-7,"errors":-1,"elapsedTime":-1}}`,
	)
	for i, p := range got {
		if p.Bytes != 10 || p.Errors != 3 || p.Checks != 5 || p.Elapsed != time.Second {
			t.Errorf("sample %d = %+v, counters decreased", i, p)
		}
	}
}

func TestTrackerDiagnostics(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", maxDiagnosticBytes) // 2 bytes per rune
	tr, _ := feed(
		`{"level":"error","msg":"first"}`,
		`{"level":"warning","msg":"not an error"}`,
		`{"level":"notice","msg":"Failed to copy: x"}`,
		`{"level":"ERROR","msg":"multi\nline   message","object":"a.txt"}`,
		`{"level":"critical","msg":"`+long+`"}`,
		"Error: unknown flag: --bogus",
		"Usage:",
		"  rclone copy source:path dest:path [flags]",
		"2026/11/04 04:06:15 ERROR : plain error",
	)
	d := tr.diagnostics()
	if len(d) != maxDiagnostics {
		t.Fatalf("diagnostics = %q", d)
	}
	if !strings.HasSuffix(d[0], "...") || len(d[0]) > maxDiagnosticBytes+len("...") || !strings.HasPrefix(d[0], "éé") {
		t.Errorf("long message not truncated cleanly: %q", d[0])
	}
	if d[1] != "Error: unknown flag: --bogus" || d[2] != "plain error" {
		t.Errorf("diagnostics = %q", d)
	}

	tr, _ = feed(`{"level":"error","msg":"multi\nline   message","object":"a.txt"}`)
	if d := tr.diagnostics(); len(d) != 1 || d[0] != "a.txt: multi line message" {
		t.Errorf("diagnostics = %q", d)
	}
}

func TestTrackerTruncatedLinesAreNotParsed(t *testing.T) {
	t.Parallel()
	var emitted int
	tr := newTracker(func(runner.Progress) { emitted++ }, nil)
	tr.line(proc.Line{Data: []byte(`{"level":"notice","stats":{"bytes":1}}`), Truncated: true})
	if emitted != 0 {
		t.Error("a truncated line was parsed")
	}
	newTracker(nil, nil).line(proc.Line{Data: []byte(`{"stats":{"bytes":1}}`)}) // nil emit is allowed
}
