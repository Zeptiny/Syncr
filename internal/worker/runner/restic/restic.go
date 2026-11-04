package restic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Supervisor runs a child process. *proc.Supervisor implements it.
type Supervisor interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// RemoteEnv turns the remotes of a job into the environment variables that
// define them for rclone (RCLONE_CONFIG_<ALIAS>_*). The implementation lives
// in internal/worker/rcloneenv; this package accepts only RCLONE_CONFIG_*
// names from it, so it can never override restic's own variables.
type RemoteEnv interface {
	Env(remotes []*syncrv1.RemoteDefinition) (map[string]string, error)
}

// PathPolicy enforces the worker's admin-configured local_roots. Resolve
// returns the cleaned, absolute, symlink-resolved path to use, or an error if
// path is not allowed. It is applied to backup sources, local repositories
// and restore targets. The implementation lives in internal/worker/localfs.
type PathPolicy interface {
	Resolve(path string) (string, error)
}

// Config holds the dependencies of the restic runners.
type Config struct {
	// ResticPath and RclonePath are the pinned binaries.
	ResticPath string
	RclonePath string

	// Supervisor runs the restic child processes.
	Supervisor Supervisor
	// Remotes defines the job's remotes for rclone.
	Remotes RemoteEnv
	// Paths enforces local_roots.
	Paths PathPolicy

	// ScratchRoot is the directory under which each job gets a private
	// scratch directory (mode 0700), used as the child's temp directory and
	// removed when the job ends.
	ScratchRoot string

	// CacheDir is the worker-controlled restic cache directory
	// (RESTIC_CACHE_DIR). Empty disables the cache (--no-cache), so restic
	// never writes to the worker user's own cache.
	CacheDir string

	// Logger receives diagnostics (never secrets). Nil discards them.
	Logger *slog.Logger
}

// New returns one runner per restic job kind, ready for runner.Registry.Register.
func New(cfg Config) ([]runner.Runner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	rcloneOpts, err := rcloneOptions(cfg.RclonePath, rcloneArgs())
	if err != nil {
		return nil, fmt.Errorf("restic: Config.RclonePath: %w", err)
	}
	e := &engine{cfg: cfg, goos: runtime.GOOS, log: cfg.Logger, rcloneOpts: rcloneOpts}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	kinds := []struct {
		kind string
		plan planFunc
	}{
		{kindInit, planInit},
		{kindBackup, planBackup},
		{kindForget, planForget},
		{kindPrune, planPrune},
		{kindCheck, planCheck},
		{kindRestore, planRestore},
		{kindSnapshots, planSnapshots},
	}
	runners := make([]runner.Runner, 0, len(kinds))
	for _, k := range kinds {
		runners = append(runners, &kindRunner{e: e, kind: k.kind, plan: k.plan})
	}
	return runners, nil
}

// engine is the state shared by the per-kind runners: configuration only, so
// it is safe for concurrent jobs.
type engine struct {
	cfg  Config
	goos string
	log  *slog.Logger
	// rcloneOpts are the restic flags that start rclone (see rcloneOptions).
	rcloneOpts []string
}

// kindRunner is the runner.Runner of one restic job kind.
type kindRunner struct {
	e    *engine
	kind string
	plan planFunc
}

// Kind implements runner.Runner.
func (r *kindRunner) Kind() string { return r.kind }

// Run implements runner.Runner.
func (r *kindRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	if job.Spec == nil {
		return res, invalid("job has no spec")
	}
	if !jobIDPattern.MatchString(job.ID) {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "invalid job ID %q", job.ID)
	}
	// Every error leaving this method is scrubbed of the job's secrets.
	sc := newScrubber(secretsOf(job.Spec))
	cmd, err := r.plan(r.e, job.Spec)
	if err != nil {
		return res, sc.Error(err)
	}
	res, err = r.e.run(ctx, r.kind, job, cmd, emit)
	if err != nil && !runner.Canceled(err) {
		err = sc.Error(err)
	}
	for i, w := range res.Warnings {
		res.Warnings[i] = sc.String(w)
	}
	res.Warnings = runner.LimitWarnings(res.Warnings)
	return res, err
}

// run executes a planned command.
func (e *engine) run(ctx context.Context, kind string, job runner.Job, cmd command, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	spec := job.Spec

	repo, viaRclone, err := e.repository(spec, cmd.repo)
	if err != nil {
		return res, err
	}
	env, err := e.baseEnv(spec, cmd.repo.GetPassword(), viaRclone)
	if err != nil {
		return res, err
	}

	scratch, cleanup, err := e.scratchDir(job.ID)
	if err != nil {
		return res, err
	}
	defer cleanup()
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		env[name] = scratch
	}

	argv := []string{"--repo=" + repo}
	if viaRclone {
		argv = append(argv, e.rcloneOpts...)
	}
	argv = append(argv, "--json")
	if e.cfg.CacheDir == "" || cmd.noCache {
		argv = append(argv, "--no-cache")
	}
	argv = append(argv, cmd.verb)
	argv = append(argv, cmd.args...)

	s := newSession(kind, emit)
	pres, err := e.cfg.Supervisor.Run(ctx, proc.Spec{
		Path:   e.cfg.ResticPath,
		Args:   argv,
		Dir:    scratch,
		Env:    env,
		Stdout: s.line,
		Stderr: s.line,
	})
	res.ExitCode = pres.ExitCode
	res.Stats = s.stats()

	switch {
	case err == nil:
	case runner.Canceled(err):
		return res, err
	default:
		if exitErr, ok := errors.AsType[*proc.ExitError](err); ok {
			return e.exitFailure(res, kind, cmd.verb, exitErr.ExitCode, s)
		}
		return res, err // *proc.StartError and collection errors
	}

	out, err := s.output(job.Spec)
	if err != nil {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic %s: %w", cmd.verb, err)
	}
	res.Output = out
	return res, nil
}

// exitFailure maps a non-zero restic exit to the job outcome. A backup that
// exits with 3 created a snapshot and so counts as succeeded with warnings.
func (e *engine) exitFailure(res runner.Result, kind, verb string, code int, s *session) (runner.Result, error) {
	if kind == kindBackup && code == exitIncomplete && s.backup != nil {
		out, err := s.backupOutput(nil)
		if err == nil {
			res.Output = out
			// The unreadable items were reported as errors; make sure the
			// warning is visible even if restic did not list them.
			res.Stats.Errors = max(res.Stats.Errors, 1)
			res.Warnings = incompleteWarnings(s)
			return res, nil
		}
	}
	return res, failure(verb, code, clip(s.detail(), maxFailureDetail))
}

// incompleteWarnings describes a backup that exited with code 3: how many
// items could not be read, then the first few reasons restic gave.
func incompleteWarnings(s *session) []string {
	n := max(s.unreadable(), 1)
	noun := "items"
	if n == 1 {
		noun = "item"
	}
	warnings := []string{fmt.Sprintf("%d %s could not be read; the snapshot is incomplete", n, noun)}
	return append(warnings, s.itemErrors...)
}

// baseEnv builds the child's variables: the job's remotes (RCLONE_CONFIG_*
// only), the repository password and the restic tuning variables.
func (e *engine) baseEnv(spec *syncrv1.JobSpec, password string, viaRclone bool) (map[string]string, error) {
	env := map[string]string{
		"RESTIC_PASSWORD": password,
		// One status line per second, matching the controller's sampling.
		"RESTIC_PROGRESS_FPS": "1",
	}
	if e.cfg.CacheDir != "" {
		env["RESTIC_CACHE_DIR"] = e.cfg.CacheDir
	}
	if !viaRclone {
		return env, nil
	}
	remote, err := e.cfg.Remotes.Env(spec.GetRemotes())
	if err != nil {
		return nil, invalid("define remotes: %w", err)
	}
	for k, v := range remote {
		if !strings.HasPrefix(strings.ToUpper(k), "RCLONE_CONFIG_") {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL,
				"remote environment variable %q is not an RCLONE_CONFIG_ variable", k)
		}
		env[k] = v
	}
	return env, nil
}

// scratchDir creates the job's private directory under ScratchRoot and
// returns it with a function that removes it.
//
// The directory is restic's working and temporary directory, and so also
// that of the rclone process restic starts for an rclone: repository. restic
// does not wait for that process to exit, so on Windows it can still hold the
// directory open when the job ends (or when a job with the same ID starts):
// proc.RemoveAll retries until it lets go.
func (e *engine) scratchDir(jobID string) (string, func(), error) {
	root := e.cfg.ScratchRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "create scratch root: %w", err)
	}
	dir := filepath.Join(root, "restic-"+jobID)
	if err := proc.RemoveAll(dir); err != nil { // left over from a crashed worker or a lingering rclone
		return "", nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "clear scratch directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "create scratch directory: %w", err)
	}
	cleanup := func() {
		if err := proc.RemoveAll(dir); err != nil {
			e.log.Warn("cannot remove job scratch directory", "job_id", jobID, "dir", dir, "error", err.Error())
		}
	}
	return dir, cleanup, nil
}
