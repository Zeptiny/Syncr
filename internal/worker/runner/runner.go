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
	"time"

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
	// Cancellation: ctx is cancelled when the controller cancels the job or
	// the worker shuts down. Run must then stop the tool promptly (proc.Run
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
