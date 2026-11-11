// Package runner defines the contract between the worker's job agent and the
// runners that execute jobs, plus a registry that maps job kinds to runners.
//
// A job kind is the name of the JobSpec.operation field that is set (see
// link.KindOf), e.g. "rclone_transfer" or "restic_backup". Adding a kind
// means adding a JobSpec operation to proto/syncr/v1/link.proto, a Runner
// that handles it, and one Register call in cmd/syncr-worker.
//
// Runner implementations live in subpackages (runner/rclone, runner/restic)
// and typically drive a child process through internal/worker/proc. They must
// not import each other; shared helpers belong in their own package.
//
// Allowed dependencies: the standard library, internal/link, internal/worker
// packages and the generated protobuf code.
package runner

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Job is the unit of work handed to a Runner.
type Job struct {
	// ID is the controller-assigned job UUID, for logs and per-job scratch
	// paths. It is never empty.
	ID string

	// Spec is the execution spec. Its operation is of the runner's kind.
	// It may carry secrets (remote credentials, repository passwords): never
	// log it, and pass secrets to child processes only through their
	// environment (proc.Spec.Env), never as arguments.
	Spec *syncrv1.JobSpec
}

// Progress is a cumulative snapshot of a running job. Each emitted value
// replaces the previous one; it is never a delta. Counters must not decrease
// during a run, except Total* fields, which may grow while the tool discovers
// more work. Zero totals mean "unknown". Rates are derived by the controller
// from consecutive snapshots, so runners do not report them.
type Progress struct {
	// Bytes transferred (rclone) or processed (restic) so far.
	Bytes int64
	// TotalBytes expected, 0 if unknown.
	TotalBytes int64
	// Files transferred or processed so far.
	Files int64
	// TotalFiles expected, 0 if unknown.
	TotalFiles int64
	// Checks done without transfer (rclone).
	Checks int64
	// Deletes performed at the destination (sync) or source (move).
	Deletes int64
	// Renames performed server-side.
	Renames int64
	// Errors are non-fatal errors so far (failed files, retried transfers).
	Errors int64
	// Elapsed is the wall-clock time since the tool process started.
	Elapsed time.Duration
}

// Proto converts p to its wire form.
func (p Progress) Proto() *syncrv1.JobStats {
	return &syncrv1.JobStats{
		Bytes:      p.Bytes,
		TotalBytes: p.TotalBytes,
		Files:      p.Files,
		TotalFiles: p.TotalFiles,
		Checks:     p.Checks,
		Deletes:    p.Deletes,
		Renames:    p.Renames,
		Errors:     p.Errors,
		Elapsed:    durationpb.New(p.Elapsed),
	}
}

// Result describes a finished run.
type Result struct {
	// Stats are the final cumulative counters. Runners fill them even when
	// Run fails, with the last values known.
	Stats Progress

	// ExitCode is the tool's exit code, or -1 if no process ran or it was
	// killed by a signal (proc.Result.ExitCode).
	ExitCode int

	// Output is the kind-specific output (e.g. the snapshot ID of a restic
	// backup). Nil for kinds without output and for failed runs.
	Output *syncrv1.JobOutput

	// Warnings are non-fatal problems of a run that returned a nil error,
	// for example "3 items could not be read" for a restic backup that
	// created its snapshot. They are reported as JobResult.warnings. Runners
	// scrub them of secrets and bound them with LimitWarnings.
	Warnings []string
}

// Bounds of Result.Warnings, matching the JobResult.warnings contract.
const (
	// MaxWarnings is the maximum number of warnings of one result.
	MaxWarnings = 10
	// MaxWarningBytes is the maximum length of one warning in bytes.
	MaxWarningBytes = 500
)

// LimitWarnings returns warnings without empty entries, each made valid
// UTF-8 (ValidText) and clipped to MaxWarningBytes on a UTF-8 boundary
// (with a "..." suffix when clipped), and at most MaxWarnings of them. It
// returns nil if nothing is left.
func LimitWarnings(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if w == "" {
			continue
		}
		if len(out) == MaxWarnings {
			break
		}
		w = ValidText(w)
		if len(w) > MaxWarningBytes {
			cut := MaxWarningBytes
			for cut > 0 && !utf8.RuneStart(w[cut]) {
				cut--
			}
			w = w[:cut] + "..."
		}
		out = append(out, w)
	}
	return out
}

// ValidText returns s with every run of bytes that is not valid UTF-8
// replaced by U+FFFD. Text taken from a tool's output goes through it
// before it is put in a link message: a tool may print bytes in another
// encoding (a file name, a remote's error body) on a line that is not
// JSON, and a message with such a string field cannot be encoded, so its
// event, and every event after it, could never be delivered.
func ValidText(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

// EmitFunc publishes a progress snapshot of the running job.
type EmitFunc func(Progress)

// Runner executes jobs of one kind.
//
// Implementations must be safe for concurrent use: the agent may run several
// jobs, including jobs of the same kind, at once. Per-job state lives inside
// Run.
type Runner interface {
	// Kind returns the job kind this runner executes: the name of a
	// JobSpec.operation field (link.IsKind must report true for it).
	Kind() string

	// Run executes job and blocks until it has finished.
	//
	// Progress: Run calls emit with cumulative snapshots whenever the tool
	// reports progress (typically about once a second). emit must only be
	// called from the goroutine executing Run — proc line callbacks run there,
	// so calling emit from them is correct — never concurrently and never
	// after Run returns. emit is cheap and does not block (the agent merges
	// snapshots under backlog), so there is no need to throttle it.
	//
	// Cancellation: ctx is cancelled when the controller cancels the job,
	// the job reaches its time limit (JobSpec.max_runtime) or the worker
	// shuts down. Run must then stop the tool promptly (proc.Run
	// does this: graceful stop, kill after the grace period) and return an
	// error, normally the *proc.CanceledError it got, which wraps
	// context.Cause(ctx). The agent treats any non-nil error returned after
	// ctx is done as "cancelled", and a nil error as "succeeded" even if ctx
	// was cancelled at the very end.
	//
	// Errors: a nil error means the job succeeded. Otherwise the job failed;
	// wrap the error with Errorf or Fail to choose the reported
	// syncrv1.JobErrorCode (see CodeOf for the defaults). Error messages are
	// shown to users and must not contain secrets.
	//
	// Run returns a Result in every case, with the final stats known so far.
	// It must not panic; the agent recovers panics but reports them as
	// internal errors.
	Run(ctx context.Context, job Job, emit EmitFunc) (Result, error)
}
