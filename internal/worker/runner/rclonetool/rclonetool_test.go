package rclonetool

import (
	"errors"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

func rule(action syncrv1.FilterAction, pattern string) *syncrv1.FilterRule {
	return &syncrv1.FilterRule{Action: action, Pattern: pattern}
}

func TestFilterLinesAndFile(t *testing.T) {
	t.Parallel()
	inc, exc := syncrv1.FilterAction_FILTER_ACTION_INCLUDE, syncrv1.FilterAction_FILTER_ACTION_EXCLUDE
	lines, err := FilterLines([]*syncrv1.FilterRule{rule(exc, "*.tmp"), rule(inc, "/photos/**"), rule(exc, "-rf --config x")})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"- *.tmp", "+ /photos/**", "- -rf --config x"}
	if !slices.Equal(lines, want) {
		t.Errorf("FilterLines = %q, want %q", lines, want)
	}
	if got := string(FiltersFile(lines)); got != "- *.tmp\n+ /photos/**\n- -rf --config x\n" {
		t.Errorf("FiltersFile = %q", got)
	}
	if got := FiltersFile(nil); len(got) != 0 {
		t.Errorf("FiltersFile(nil) = %q", got)
	}
	for name, bad := range map[string][]*syncrv1.FilterRule{
		"no action":    {rule(syncrv1.FilterAction_FILTER_ACTION_UNSPECIFIED, "x")},
		"empty":        {rule(inc, "  ")},
		"line break":   {rule(inc, "a\nb")},
		"NUL":          {rule(inc, "a\x00")},
		"too long":     {rule(inc, strings.Repeat("x", MaxPatternBytes+1))},
		"too many":     slices.Repeat([]*syncrv1.FilterRule{rule(inc, "x")}, MaxFilters+1),
		"carriage ret": {rule(exc, "a\rb")},
	} {
		if _, err := FilterLines(bad); err == nil {
			t.Errorf("%s: FilterLines succeeded", name)
		}
	}
}

func TestOptionFlags(t *testing.T) {
	t.Parallel()
	ptr32 := func(v int32) *int32 { return &v }
	ptr64 := func(v int64) *int64 { return &v }
	for _, tt := range []struct {
		v    *int32
		want string
		ok   bool
	}{{nil, "", true}, {ptr32(1), "--transfers=1", true}, {ptr32(MaxTransfers), "--transfers=64", true}, {ptr32(0), "", false}, {ptr32(MaxTransfers + 1), "", false}} {
		got, err := TransfersFlag("transfers", tt.v)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("TransfersFlag(%v) = %q, %v", tt.v, got, err)
		}
	}
	for _, tt := range []struct {
		v    *int64
		want string
		ok   bool
	}{{nil, "", true}, {ptr64(0), "", true}, {ptr64(1048576), "--bwlimit=1048576B", true}, {ptr64(-1), "", false}} {
		got, err := BandwidthFlag("bandwidth_limit_bytes_per_second", tt.v)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("BandwidthFlag(%v) = %q, %v", tt.v, got, err)
		}
	}
	if got := BaseFlags(); !slices.Equal(got, []string{"--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE"}) {
		t.Errorf("BaseFlags = %q", got)
	}
}

func TestClassifyExitAndFailure(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]syncrv1.JobErrorCode{
		1:  syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		2:  syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC,
		3:  syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
		4:  syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
		5:  syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY,
		6:  syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		7:  syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		8:  syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED,
		10: syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED,
		-1: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		42: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
	} {
		if c := ClassifyExit(code); c.Success || c.Code != want || c.Summary == "" {
			t.Errorf("ClassifyExit(%d) = %+v, want code %v", code, c, want)
		}
	}
	for _, code := range []int{0, 9} {
		if !ClassifyExit(code).Success {
			t.Errorf("ClassifyExit(%d) is not a success", code)
		}
	}
	exit := &proc.ExitError{Path: "rclone", ExitCode: 3}
	err := Failure("lsf", 3, []string{"error listing: directory not found"}, exit)
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND || !errors.Is(err, exit) ||
		err.Error() != "rclone lsf failed: directory not found (exit code 3): error listing: directory not found" {
		t.Errorf("Failure = %v (%v)", err, runner.CodeOf(err))
	}
	if err := Failure("sync", -1, nil, exit); err.Error() != "rclone sync failed: rclone was terminated by a signal" {
		t.Errorf("Failure of a signal = %v", err)
	}
}

// TestTracker reads stats and errors from rclone v1.75.2 JSON log lines, and
// scrubs a secret out of an error line.
func TestTracker(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-secret"
	var emitted []runner.Progress
	tr := NewTracker(func(p runner.Progress) { emitted = append(emitted, p) }, scrub.New([]string{secret}))
	var observed []string
	tr.Observe = func(l LogLine) { observed = append(observed, l.Level) }
	for _, l := range []string{
		`{"level":"notice","msg":"stats","stats":{"bytes":6,"checks":0,"deletes":0,"elapsedTime":0.5,"errors":0,"totalBytes":6,"totalTransfers":3,"transfers":3}}`,
		`{"level":"error","msg":"login as ` + secret + ` failed","object":"a.txt"}`,
		`{"level":"notice","msg":"stats","stats":{"bytes":"x","checks":3,"deletes":1,"elapsedTime":1.5,"transfers":2}}`,
		`2026/11/14 01:54:00 CRITICAL: Fatal error: boom`,
		`not json and no error`,
	} {
		tr.Line(proc.Line{Data: []byte(l)})
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted %d samples", len(emitted))
	}
	// Counters never decrease within a run; totals follow rclone.
	p := tr.Progress()
	if p.Bytes != 6 || p.Files != 3 || p.Checks != 3 || p.Deletes != 1 || p.TotalFiles != 0 || p.Elapsed.Seconds() != 1.5 {
		t.Errorf("progress = %+v", p)
	}
	want := []string{"a.txt: login as [redacted] failed", "Fatal error: boom"}
	if d := tr.Diagnostics(); !slices.Equal(d, want) {
		t.Errorf("diagnostics = %q, want %q", d, want)
	}
	if !slices.Equal(observed, []string{"notice", "error", "notice"}) {
		t.Errorf("observed = %q", observed)
	}
}
