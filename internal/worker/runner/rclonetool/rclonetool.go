// Package rclonetool holds what the runners that start the pinned rclone
// share: the flags that make rclone log JSON lines with its stats, the
// checks and limits of rclone filter rules and options, the meaning of
// rclone's exit codes, and the reader of its JSON log (Tracker), which turns
// stats lines into runner.Progress and keeps the last error lines, scrubbed
// of secrets, for failure messages.
//
// The rclone runner (transfers and two-way syncs) and the restic runner's
// backups of a folder of a remote (its rclone listing, sync and mount) use
// it, so both read rclone the same way and classify its failures alike.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/worker/proc, internal/worker/runner and
// internal/worker/runner/scrub.
package rclonetool

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Limits of rclone options and filter rules. They keep a job from exhausting
// the worker (thousands of parallel transfers) while staying far above
// useful values.
const (
	MaxTransfers       = 64
	MaxCheckers        = 256
	MaxRetries         = 100
	MaxLowLevelRetries = 1000
	MaxFilters         = 1000
	MaxPatternBytes    = 4096
	MaxDuration        = 366 * 24 * time.Hour
)

// StatsInterval is how often rclone reports progress.
const StatsInterval = "1s"

// BaseFlags make rclone log JSON lines on stderr and print its stats as one
// such line every StatsInterval:
//
//   - --use-json-log: every log line is a JSON object; stats lines carry a
//     "stats" object with the counters.
//   - --log-level=NOTICE: rclone's default, made explicit; per-file INFO
//     lines ("Copied (new)") are not needed and can be huge.
//   - --stats=1s: periodic stats (rclone also prints them once at exit).
//   - --stats-log-level=NOTICE: stats are logged at INFO by default, which
//     NOTICE would hide.
func BaseFlags() []string {
	return []string{
		"--use-json-log",
		"--log-level=NOTICE",
		"--stats=" + StatsInterval,
		"--stats-log-level=NOTICE",
	}
}

// FilterLines checks filter rules and returns them as rclone filter lines
// ("+ pattern" or "- pattern"), in order: the values of --filter, and the
// lines of a filters file (--filter-from, --filters-file).
func FilterLines(rules []*syncrv1.FilterRule) ([]string, error) {
	if len(rules) > MaxFilters {
		return nil, fmt.Errorf("at most %d filter rules are allowed", MaxFilters)
	}
	lines := make([]string, 0, len(rules))
	for i, r := range rules {
		var sign string
		switch r.GetAction() {
		case syncrv1.FilterAction_FILTER_ACTION_INCLUDE:
			sign = "+"
		case syncrv1.FilterAction_FILTER_ACTION_EXCLUDE:
			sign = "-"
		default:
			return nil, fmt.Errorf("filter rule %d: action must be include or exclude", i)
		}
		p := r.GetPattern()
		switch {
		case strings.TrimSpace(p) == "":
			return nil, fmt.Errorf("filter rule %d: empty pattern", i)
		case len(p) > MaxPatternBytes:
			return nil, fmt.Errorf("filter rule %d: pattern longer than %d bytes", i, MaxPatternBytes)
		case strings.ContainsAny(p, "\x00\r\n"):
			return nil, fmt.Errorf("filter rule %d: pattern contains a line break or NUL byte", i)
		}
		lines = append(lines, sign+" "+p)
	}
	return lines, nil
}

// FiltersFile returns the content of a filters file with lines (from
// FilterLines), one per line. No lines give an empty file. The same lines
// always give the same bytes.
func FiltersFile(lines []string) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// TransfersFlag returns --transfers for v (nil: none), or an error when v is
// outside 1..MaxTransfers.
func TransfersFlag(name string, v *int32) (string, error) {
	if v == nil {
		return "", nil
	}
	if *v < 1 || *v > MaxTransfers {
		return "", fmt.Errorf("%s must be between 1 and %d", name, MaxTransfers)
	}
	return "--transfers=" + strconv.FormatInt(int64(*v), 10), nil
}

// BandwidthFlag returns --bwlimit for a limit in bytes per second (nil or 0:
// none), or an error for a negative one. A bare number would mean KiB/s to
// rclone; the B suffix means bytes.
func BandwidthFlag(name string, v *int64) (string, error) {
	switch {
	case v == nil || *v == 0:
		return "", nil
	case *v < 0:
		return "", errors.New(name + " must not be negative")
	}
	return "--bwlimit=" + strconv.FormatInt(*v, 10) + "B", nil
}

// ExitClass describes an rclone exit code.
type ExitClass struct {
	// Success reports an exit that counts as success.
	Success bool
	// Code is the job error code of a failure.
	Code syncrv1.JobErrorCode
	// Summary describes a failure in a few words.
	Summary string
}

// ClassifyExit maps rclone's documented exit codes
// (https://rclone.org/docs/#exit-code, lib/exitcode) to job outcomes.
func ClassifyExit(exit int) ExitClass {
	fail := func(code syncrv1.JobErrorCode, summary string) ExitClass {
		return ExitClass{Code: code, Summary: summary}
	}
	switch exit {
	case 0:
		return ExitClass{Success: true}
	case 1:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "error")
	case 2:
		// Syncr builds the arguments, so this means a value slipped past
		// validation.
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "rclone rejected the arguments")
	case 3:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found")
	case 4:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "file not found")
	case 5:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "temporary error, retrying may help")
	case 6:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "errors that retrying will not fix")
	case 7:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "fatal error, retrying will not help")
	case 8:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "transfer limit (max_transfer_bytes) reached")
	case 9:
		// "Operation successful, but no files transferred" is only reported
		// with --error-on-no-transfer, which Syncr does not pass.
		return ExitClass{Success: true}
	case 10:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "duration limit (max_duration) reached")
	case -1:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "rclone was terminated by a signal")
	}
	return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "unexpected exit code")
}

// ExitError is a failure message built from rclone's output; it unwraps to
// the supervisor's error (a *proc.ExitError).
type ExitError struct {
	Msg string
	Err error
}

// Error implements error.
func (e *ExitError) Error() string { return e.Msg }

// Unwrap returns the supervisor's error.
func (e *ExitError) Unwrap() error { return e.Err }

// Failure returns the runner error of an rclone command (cmd, e.g. "sync")
// that exited with exitCode, which ClassifyExit says is a failure: "rclone
// <cmd> failed: <summary> (exit code N): <diagnostics>", with the code
// ClassifyExit gives. diagnostics must be scrubbed already (Tracker's are).
// err is the supervisor's error, which the result unwraps to.
func Failure(cmd string, exitCode int, diagnostics []string, err error) error {
	ec := ClassifyExit(exitCode)
	var b strings.Builder
	fmt.Fprintf(&b, "rclone %s failed: %s", cmd, ec.Summary)
	if exitCode >= 0 {
		fmt.Fprintf(&b, " (exit code %d)", exitCode)
	}
	if len(diagnostics) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(diagnostics, "; "))
	}
	return runner.Fail(ec.Code, &ExitError{Msg: b.String(), Err: err})
}
