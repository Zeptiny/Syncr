// Package rclone implements the "rclone_transfer" job kind: rclone copy,
// sync and move between two locations, each a remote defined in the job spec
// or a local path inside the worker's local_roots.
//
// The runner never builds a connection string or a free-form flag:
//
//   - remotes reach rclone as RCLONE_CONFIG_* environment variables built by
//     rcloneenv, in a per-job scratch directory with an empty config file,
//     so the host's rclone configuration is never read;
//   - every RcloneTransferOptions field maps to one fixed flag after range
//     checks (see buildArgs);
//   - paths follow "--", remote paths are "<alias>:<path>" with a validated
//     alias, and local paths must pass the local path policy.
//
// Progress comes from rclone's JSON log (--use-json-log, stats every
// second); unknown keys are ignored. Exit codes map to job error codes, and
// the last error lines are added to failure messages with secret values
// scrubbed.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/worker/proc, internal/worker/runner and internal/worker/rcloneenv.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Kind is the job kind this runner executes (the JobSpec.operation field).
const Kind = "rclone_transfer"

// Supervisor runs child processes; *proc.Supervisor implements it.
type Supervisor interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// EnvBuilder validates remote definitions and builds the child environment
// inside a scratch directory; *rcloneenv.Builder implements it.
type EnvBuilder interface {
	Build(dir string, remotes []*syncrv1.RemoteDefinition) (*rcloneenv.Env, error)
}

// PathPolicy checks local paths against the worker's local_roots and returns
// the path to use; *localfs.Policy implements it.
type PathPolicy interface {
	Resolve(path string) (string, error)
}

// Config holds the runner's dependencies.
type Config struct {
	// Binary is the rclone executable, normally an absolute path resolved by
	// tool provisioning.
	Binary string
	// Supervisor runs rclone.
	Supervisor Supervisor
	// Env builds remotes and isolation variables.
	Env EnvBuilder
	// Paths checks local locations.
	Paths PathPolicy
	// ScratchDir is an absolute directory under which each job gets a
	// private directory (rclone config, cache), removed when the job ends.
	// It is created if missing.
	ScratchDir string
	// Logger receives operational warnings (never job secrets). Nil
	// discards them.
	Logger *slog.Logger
}

// Runner runs rclone transfers. It holds no per-job state and is safe for
// concurrent use.
type Runner struct {
	cfg Config
}

var _ runner.Runner = (*Runner)(nil)

// New returns a Runner, or an error if a dependency is missing.
func New(cfg Config) (*Runner, error) {
	var errs []error
	if cfg.Binary == "" {
		errs = append(errs, errors.New("rclone binary path is empty"))
	}
	if cfg.Supervisor == nil {
		errs = append(errs, errors.New("supervisor is nil"))
	}
	if cfg.Env == nil {
		errs = append(errs, errors.New("env builder is nil"))
	}
	if cfg.Paths == nil {
		errs = append(errs, errors.New("path policy is nil"))
	}
	if !filepath.IsAbs(cfg.ScratchDir) {
		errs = append(errs, fmt.Errorf("scratch dir %q is not absolute", cfg.ScratchDir))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("rclone runner: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{cfg: cfg}, nil
}

// Kind implements runner.Runner.
func (*Runner) Kind() string { return Kind }

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	transfer := job.Spec.GetRcloneTransfer()
	if transfer == nil {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "job spec is not an rclone transfer")
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("rclone: not started: %w", context.Cause(ctx))
	}

	dir, err := r.scratchDir(job.ID)
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}
	defer r.removeScratch(dir)

	env, err := r.cfg.Env.Build(dir, job.Spec.GetRemotes())
	if err != nil {
		code := syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC
		if errors.Is(err, rcloneenv.ErrLocalPath) {
			code = syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED
		}
		return res, runner.Fail(code, err)
	}
	src, err := r.location("source", transfer.GetSource(), env)
	if err != nil {
		return res, err
	}
	dst, err := r.location("destination", transfer.GetDestination(), env)
	if err != nil {
		return res, err
	}
	args, err := buildArgs(transfer.GetMode(), transfer.GetOptions(), src, dst)
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, err)
	}

	t := newTracker(emit)
	pres, err := r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path:   r.cfg.Binary,
		Args:   args,
		Dir:    dir,
		Env:    env.Vars(),
		Stdout: t.line,
		Stderr: t.line,
	})
	res.ExitCode = pres.ExitCode
	res.Stats = t.progress
	res.Stats.Elapsed = max(res.Stats.Elapsed, pres.Duration)
	return res, r.outcome(err, pres, t, env, args[0])
}

// outcome maps the supervisor's result to the runner's error.
func (r *Runner) outcome(err error, pres proc.Result, t *tracker, env *rcloneenv.Env, cmd string) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*proc.CanceledError](err); ok {
		return err
	}
	if _, ok := errors.AsType[*proc.StartError](err); ok {
		return runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE, fmt.Errorf("cannot start rclone: %w", err))
	}
	if _, ok := errors.AsType[*proc.ExitError](err); !ok {
		if pres.ExitCode == 0 {
			// rclone succeeded, but a grandchild kept its output open past
			// the grace period. The transfer is complete.
			r.cfg.Logger.Warn("rclone output not fully collected", "error", err.Error())
			return nil
		}
		return runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}

	ec := classifyExit(pres.ExitCode)
	if ec.success {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rclone %s failed: %s", cmd, ec.summary)
	if pres.ExitCode >= 0 {
		fmt.Fprintf(&b, " (exit code %d)", pres.ExitCode)
	}
	if d := t.diagnostics(); len(d) > 0 {
		b.WriteString(": ")
		b.WriteString(env.Scrubber().Scrub(strings.Join(d, "; ")))
	}
	return runner.Fail(ec.code, &exitError{msg: b.String(), err: err})
}

// exitError is a failure message built from rclone's output; it unwraps to
// the supervisor's *proc.ExitError.
type exitError struct {
	msg string
	err error
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) Unwrap() error { return e.err }

// location validates loc and returns its rclone path.
func (r *Runner) location(role string, loc *syncrv1.Location, env *rcloneenv.Env) (string, error) {
	if loc == nil {
		return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "%s is missing", role)
	}
	path := loc.GetPath()
	if strings.ContainsRune(path, 0) {
		return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "%s path contains a NUL byte", role)
	}
	alias := loc.GetRemote()
	if alias == "" {
		resolved, err := r.cfg.Paths.Resolve(path)
		if err != nil {
			return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "%s: %w", role, err)
		}
		if strings.HasPrefix(resolved, "-") || !filepath.IsAbs(resolved) {
			// The policy returns absolute paths; never let anything else
			// through, as rclone would parse "name:" as a remote.
			return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "%s: path policy returned a non-absolute path", role)
		}
		return resolved, nil
	}
	if _, ok := env.BackendType(alias); !ok {
		return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "%s remote %q is not defined in the job", role, alias)
	}
	return rcloneenv.RemotePath(alias, path), nil
}

// scratchDir creates the job's private directory under cfg.ScratchDir.
func (r *Runner) scratchDir(jobID string) (string, error) {
	if err := os.MkdirAll(r.cfg.ScratchDir, 0o700); err != nil {
		return "", fmt.Errorf("create scratch root: %w", err)
	}
	dir, err := os.MkdirTemp(r.cfg.ScratchDir, "rclone-"+safeName(jobID)+"-*")
	if err != nil {
		return "", fmt.Errorf("create job scratch dir: %w", err)
	}
	return dir, nil
}

func (r *Runner) removeScratch(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		r.cfg.Logger.Warn("cannot remove job scratch dir", "dir", dir, "error", err.Error())
	}
}

// safeName keeps the characters of a job ID that are safe in a file name
// on every platform, at most 36 of them.
func safeName(id string) string {
	var b strings.Builder
	for _, c := range id {
		if b.Len() == 36 {
			break
		}
		if c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			b.WriteRune(c)
		}
	}
	return b.String()
}
