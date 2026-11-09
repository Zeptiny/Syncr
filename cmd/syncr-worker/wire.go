package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
	"github.com/Zeptiny/Syncr/internal/worker/runner/restic"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// maxToolLineBytes is the longest line of tool output the supervisor
// delivers whole. restic prints its snapshot list and the forget result as a
// single JSON line that grows with the repository, so the supervisor's 1 MiB
// default is too small for a repository with many snapshots.
const maxToolLineBytes = 32 << 20

// Tool names as pinned in the manifest.
const (
	toolRclone = "rclone"
	toolRestic = "restic"
)

// workerDeps are the process-level inputs of buildWorker that tests replace.
type workerDeps struct {
	// paths locates the default data directory.
	paths paths.Env
	// bundledDir is searched for tools shipped next to the executable; empty
	// disables the bundled provider.
	bundledDir string
	// tools overrides the manifest, the network and the version runner.
	tools toolsOverrides
}

// worker is the assembled job-execution side of the worker: everything the
// link (startLink) needs to run jobs.
type worker struct {
	// registry holds the runners of every job kind this worker executes.
	registry *runner.Registry
	// tools are the verified rclone and restic binaries, by tool name.
	tools map[string]tools.Resolved
	// supervisor starts all child processes.
	supervisor *proc.Supervisor
	// policy is the local_roots policy shared by the runners.
	policy *localfs.Policy
	// dataDir, scratchDir and cacheDir are the directories in use.
	dataDir, scratchDir, cacheDir string
}

// buildWorker wires the worker from cfg: it resolves the pinned tools,
// builds the local_roots policy and the process supervisor, and registers
// the rclone runner and the restic runners. It creates the data, scratch and
// cache directories. Every failure is returned with an explanation an
// operator can act on; nothing is started.
func buildWorker(ctx context.Context, cfg workerconfig.Config, deps workerDeps, log *slog.Logger) (*worker, error) {
	dataDir, err := resolveDataDir(cfg, deps.paths)
	if err != nil {
		return nil, err
	}
	w := &worker{
		dataDir:    dataDir,
		scratchDir: paths.ScratchDir(dataDir),
		cacheDir:   paths.CacheDir(dataDir),
	}
	resticCache := filepath.Join(w.cacheDir, "restic")
	for _, dir := range []string{w.scratchDir, resticCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create worker directory %s: %w (set data_dir or SYNCR_DATA_DIR to a directory this user can write)", dir, err)
		}
	}
	toolsDir := cfg.Tools.Dir
	if toolsDir == "" {
		toolsDir = paths.ToolsDir(dataDir)
	}
	log.Info("worker directories", "data_dir", dataDir, "scratch_dir", w.scratchDir, "cache_dir", w.cacheDir, "tools_dir", toolsDir)

	w.policy, err = localfs.NewPolicy(cfg.LocalRoots)
	if err != nil {
		return nil, fmt.Errorf("local_roots: %w", err)
	}
	if roots := w.policy.Roots(); len(roots) == 0 {
		log.Warn("no local_roots configured: jobs cannot read or write local paths; " +
			"set local_roots (SYNCR_LOCAL_ROOTS) to the directories jobs may use")
	} else {
		log.Info("local roots", "roots", roots)
	}

	w.supervisor = proc.New(proc.Options{MaxLineBytes: maxToolLineBytes, Logger: log})

	w.tools, err = resolveTools(ctx, cfg, deps, w.supervisor, toolsDir, log)
	if err != nil {
		return nil, err
	}

	w.registry = runner.NewRegistry()
	if err := registerRunners(w, resticCache, log); err != nil {
		return nil, err
	}
	return w, nil
}

// resolveDataDir returns cfg.DataDir or the platform default.
func resolveDataDir(cfg workerconfig.Config, env paths.Env) (string, error) {
	if cfg.DataDir != "" {
		return cfg.DataDir, nil
	}
	dir, err := paths.DataDir(paths.DefaultMode(env), env)
	if err != nil {
		return "", fmt.Errorf("find the worker data directory: %w (set data_dir or SYNCR_DATA_DIR)", err)
	}
	return dir, nil
}

// resolveTools finds and verifies rclone and restic, logging where each came
// from and any candidate that was found but rejected. It reports every
// missing tool at once.
func resolveTools(ctx context.Context, cfg workerconfig.Config, deps workerDeps, sup *proc.Supervisor, toolsDir string, log *slog.Logger) (map[string]tools.Resolved, error) {
	var versionRunner tools.Runner = sup
	if deps.tools.runner != nil {
		versionRunner = deps.tools.runner
	}
	manifest := deps.tools.pinned()
	resolver, err := tools.New(tools.Config{
		Manifest: manifest,
		Configured: map[string]string{
			toolRclone: cfg.Tools.RclonePath,
			toolRestic: cfg.Tools.ResticPath,
		},
		BundledDir: deps.bundledDir,
		ToolsDir:   toolsDir,
		Download:   cfg.Tools.Download,
		Runner:     versionRunner,
		HTTPClient: deps.tools.httpClient,
		BaseURL:    deps.tools.baseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("set up tool provisioning: %w", err)
	}

	resolved := make(map[string]tools.Resolved, 2)
	var failures []error
	for _, tool := range []string{toolRclone, toolRestic} {
		res, err := resolver.Resolve(ctx, tool)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			failures = append(failures, fmt.Errorf("%s: %w", tool, err))
			continue
		}
		for _, rej := range res.Rejected {
			log.Warn("tool candidate rejected", "tool", tool, "source", string(rej.Source), "error", rej.Err.Error())
		}
		log.Info("tool resolved", "tool", tool, "version", res.Version, "source", string(res.Source), "path", res.Path)
		resolved[tool] = res
	}
	if len(failures) > 0 {
		return nil, &toolsUnavailableError{
			err:      errors.Join(failures...),
			manifest: manifest,
			toolsDir: toolsDir,
			download: cfg.Tools.Download,
		}
	}
	return resolved, nil
}

// toolsUnavailableError explains how to provide the missing tools.
type toolsUnavailableError struct {
	err      error
	manifest tools.Manifest
	toolsDir string
	download bool
}

func (e *toolsUnavailableError) Error() string {
	var pins []string
	for _, s := range e.manifest.Tools {
		pins = append(pins, s.Name+" "+s.Version)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "required tools are not available (this release pins %s):\n", strings.Join(pins, ", "))
	for line := range strings.SplitSeq(e.err.Error(), "\n") {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("fix this in one of these ways:\n")
	fmt.Fprintf(&b, "  - install the pinned tools: %s tools fetch --dir %s\n", name, e.toolsDir)
	if !e.download {
		b.WriteString("  - or let the worker download them at startup: tools.download: true (SYNCR_TOOLS_DOWNLOAD=true)\n")
	}
	b.WriteString("  - or point tools.rclone_path / tools.restic_path (SYNCR_RCLONE_PATH / SYNCR_RESTIC_PATH) at binaries of the pinned versions\n")
	b.WriteString("run '" + name + " tools status' to see what each source offers")
	return b.String()
}

func (e *toolsUnavailableError) Unwrap() error { return e.err }

// registerRunners builds the rclone runner and the restic runners and
// registers them. The rcloneenv builder is both runners' remote definition
// port and the localfs policy their path port, so every remote and every
// local path a job names is validated by the same code in both engines.
func registerRunners(w *worker, resticCache string, log *slog.Logger) error {
	remotes := rcloneenv.NewBuilder(w.policy)
	rcloneBin, resticBin := w.tools[toolRclone].Path, w.tools[toolRestic].Path

	rcloneRunner, err := rclone.New(rclone.Config{
		Binary:     rcloneBin,
		Supervisor: w.supervisor,
		Env:        remotes,
		Paths:      w.policy,
		ScratchDir: filepath.Join(w.scratchDir, "rclone"),
		Logger:     log,
	})
	if err != nil {
		return fmt.Errorf("set up the rclone runner: %w", err)
	}
	resticRunners, err := restic.New(restic.Config{
		ResticPath:  resticBin,
		RclonePath:  rcloneBin,
		Supervisor:  w.supervisor,
		Remotes:     remotes,
		Paths:       w.policy,
		ScratchRoot: filepath.Join(w.scratchDir, "restic"),
		CacheDir:    resticCache,
		Logger:      log,
	})
	if err != nil {
		return fmt.Errorf("set up the restic runners: %w", err)
	}
	if err := w.registry.Register(append([]runner.Runner{rcloneRunner}, resticRunners...)...); err != nil {
		return fmt.Errorf("register runners: %w", err)
	}
	return nil
}
