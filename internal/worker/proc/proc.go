// Package proc supervises the external tool processes (rclone, restic) that
// run jobs on a worker.
//
// A Supervisor starts one child process per Run call and guarantees:
//
//   - Minimal environment. The child sees only an allowlist of the worker's
//     environment (PATH, HOME, temp dirs, locale, proxy and CA settings, and
//     the variables Windows needs) plus the variables the caller passes in
//     Spec.Env. The worker's own secrets (enrollment token, SYNCR_*) never
//     reach children.
//   - Line streaming. stdout and stderr are split into lines (handling CRLF
//     and lines up to Options.MaxLineBytes) and passed to callbacks, which run
//     sequentially on the goroutine that called Run.
//   - Cancellation. When ctx is done the child is asked to stop (SIGINT to
//     the child on Unix; on Windows it is killed immediately) and killed
//     after the grace period if it is still running. Pipes left open by
//     orphaned grandchildren are closed after the same grace period, so Run
//     never hangs.
//   - Whole process trees. A run is over only when every process the child
//     started is gone too: once the child has exited (on its own, after the
//     stop request or killed after the grace period), what is left of its
//     tree is killed. On Linux that is the child's process group, signaled
//     while the exited child is not yet reaped, so the group ID cannot have
//     been reused (see tree_linux.go); on Windows, a Job Object of the run's
//     own, terminated on cancel and again once the child has exited. Other
//     Unix systems stop only the child. A process that left the group
//     (setsid, setpgid) is not stopped.
//   - Typed errors. Run returns *StartError, *ExitError or *CanceledError,
//     and always reports the exit code in Result.
//   - No orphans. A child dies with the worker, even when the worker itself
//     is killed (SIGKILL, OOM, crash): on Linux through the parent-death
//     signal, on Windows through a kill-on-close Job Object. Other Unix
//     systems lack both, and there a killed worker's children keep running.
//
// On Unix the child runs in its own process group, so a Ctrl-C in the
// worker's terminal reaches only the worker, which then stops its children
// in an orderly way.
//
// Allowed dependencies: the standard library, golang.org/x/sys/unix on
// Linux and golang.org/x/sys/windows on Windows.
package proc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Defaults for Options.
const (
	DefaultGracePeriod  = 10 * time.Second
	DefaultMaxLineBytes = 1 << 20 // 1 MiB
)

// lineBuffer is the number of lines buffered between the pipe readers and
// the callback loop before the child is back-pressured.
const lineBuffer = 64

// Options configures a Supervisor. The zero value is valid.
type Options struct {
	// Environ returns the worker's environment in os.Environ form; only
	// allowlisted entries are passed to children. Nil means os.Environ.
	Environ func() []string

	// GracePeriod is how long a cancelled child may take to exit before it
	// is killed, and how long Run waits for pipes held open by orphaned
	// grandchildren. Zero means DefaultGracePeriod.
	GracePeriod time.Duration

	// MaxLineBytes caps the length of a delivered line; longer lines are
	// truncated (Line.Truncated) and the rest of the line is discarded.
	// Zero means DefaultMaxLineBytes.
	MaxLineBytes int

	// Logger receives operational warnings, such as a child that could not
	// be tied to the worker's lifetime. Nil discards them.
	Logger *slog.Logger
}

// Supervisor starts and supervises child processes. It holds no per-run
// state and is safe for concurrent use. A worker should use one Supervisor
// for all its children.
type Supervisor struct {
	environ      func() []string
	gracePeriod  time.Duration
	maxLineBytes int
	log          *slog.Logger
	life         lifetime
}

// New returns a Supervisor configured by opts.
func New(opts Options) *Supervisor {
	s := &Supervisor{
		environ:      opts.Environ,
		gracePeriod:  opts.GracePeriod,
		maxLineBytes: opts.MaxLineBytes,
		log:          opts.Logger,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.environ == nil {
		s.environ = os.Environ
	}
	if s.gracePeriod <= 0 {
		s.gracePeriod = DefaultGracePeriod
	}
	if s.maxLineBytes <= 0 {
		s.maxLineBytes = DefaultMaxLineBytes
	}
	return s
}

// Line is one line of child output, without the trailing "\n" or "\r\n".
type Line struct {
	// Data is the line content. The callback owns it and may retain it.
	Data []byte
	// Truncated reports that the line exceeded Options.MaxLineBytes and Data
	// holds only its first MaxLineBytes bytes.
	Truncated bool
}

// LineFunc receives output lines. Calls for both streams are made
// sequentially from the goroutine that called Run, never concurrently, and
// never after Run returns; it must not block for long, because the child is
// back-pressured while it runs.
type LineFunc func(Line)

// Spec describes one child process.
type Spec struct {
	// Path is the executable to run. It should be absolute (as resolved by
	// tool provisioning); a bare name is looked up in the worker's PATH.
	Path string

	// Args are the arguments, without the program name.
	Args []string

	// Dir is the working directory; empty means the worker's.
	Dir string

	// Env holds variables added to the child's minimal environment,
	// overriding allowlisted variables of the same name (case-insensitively
	// on Windows). This is how remotes reach rclone (RCLONE_CONFIG_*) and
	// passwords reach restic (RESTIC_PASSWORD). Names must be non-empty and
	// contain no "=" or NUL; values must contain no NUL.
	Env map[string]string

	// Stdout and Stderr receive the child's output line by line. Nil
	// discards the stream.
	Stdout LineFunc
	Stderr LineFunc

	// StdoutChunks delivers standard output as it is read, in chunks of
	// at most Options.MaxLineBytes, instead of line by line: for a tool
	// that streams one long document without newlines (restic find
	// --json), which would otherwise arrive only when the tool exits. A
	// chunk may end anywhere, even inside a UTF-8 sequence; Line.Truncated
	// is never set.
	StdoutChunks bool

	// GracePeriod overrides Options.GracePeriod for this run when positive.
	GracePeriod time.Duration
}

// Result describes a finished child.
type Result struct {
	// ExitCode is the child's exit code; -1 if it did not start or was
	// terminated by a signal.
	ExitCode int
	// Duration is the wall-clock time from start to exit.
	Duration time.Duration
}

// Run starts the child described by spec and blocks until it has exited and
// all its output has been delivered.
//
// Errors:
//   - *StartError: the process could not be started (missing binary,
//     permissions, invalid Spec). No callbacks were called.
//   - *CanceledError: ctx was done before the child exited on its own; it
//     unwraps to context.Cause(ctx).
//   - *ExitError: the child exited unsuccessfully on its own.
//   - any other error: the child exited successfully but output could not be
//     fully collected (e.g. a grandchild kept the pipes open past the grace
//     period; errors.Is(err, exec.ErrWaitDelay)).
//
// If a callback panics, the child is killed and the panic propagates after
// the child's resources are released in the background.
func (s *Supervisor) Run(ctx context.Context, spec Spec) (Result, error) {
	res := Result{ExitCode: -1}
	name := filepath.Base(spec.Path)
	if spec.Path == "" {
		return res, &StartError{Path: spec.Path, Err: errors.New("empty path")}
	}
	env, err := buildEnv(s.environ(), spec.Env)
	if err != nil {
		return res, &StartError{Path: spec.Path, Err: err}
	}

	grace := s.gracePeriod
	if spec.GracePeriod > 0 {
		grace = spec.GracePeriod
	}

	t := newTree()
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = env // never nil: a nil Env would inherit the worker's environment
	cmd.Dir = spec.Dir
	cmd.WaitDelay = grace
	configureCmd(cmd, t)

	lines := make(chan streamLine, lineBuffer)
	var stdout, stderr *lineWriter
	if spec.Stdout != nil {
		stdout = newLineWriter(spec.Stdout, s.maxLineBytes, lines)
		stdout.chunks = spec.StdoutChunks
		cmd.Stdout = stdout
	}
	if spec.Stderr != nil {
		stderr = newLineWriter(spec.Stderr, s.maxLineBytes, lines)
		cmd.Stderr = stderr
	}

	start := time.Now()
	waitDone, err := s.life.startChild(cmd, t, s.log)
	if err != nil {
		if ctx.Err() != nil {
			return res, &CanceledError{Path: spec.Path, ExitCode: -1, Cause: context.Cause(ctx)}
		}
		return res, &StartError{Path: spec.Path, Err: err}
	}

	// If a callback panics before Wait returned, kill the child and keep
	// draining in the background so the copy goroutines and Wait can finish.
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			go drain(lines, waitDone)
		}
	}()

	var waitErr error
	for !waited {
		select {
		case l := <-lines:
			l.deliver()
		case waitErr = <-waitDone:
			waited = true
		}
	}
	// Wait has returned, so the copy goroutines have finished every Write:
	// what remains is buffered in the channel or pending in the writers.
	for drained := false; !drained; {
		select {
		case l := <-lines:
			l.deliver()
		default:
			drained = true
		}
	}
	stdout.flush()
	stderr.flush()

	res.Duration = time.Since(start)
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case waitErr == nil:
		return res, nil
	case ctx.Err() != nil:
		return res, &CanceledError{Path: spec.Path, ExitCode: res.ExitCode, Cause: context.Cause(ctx)}
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return res, &ExitError{Path: spec.Path, ExitCode: res.ExitCode, Err: exitErr}
	}
	return res, fmt.Errorf("proc: %s: %w", name, waitErr)
}

// drain discards lines until Wait has returned. It runs only after a
// callback panicked.
func drain(lines <-chan streamLine, waitDone <-chan error) {
	for {
		select {
		case <-lines:
		case err := <-waitDone:
			_ = err // Run already panicked; there is no caller to report to
			return
		}
	}
}
