package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/datadir"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/upgrade"
)

// runCmd runs the worker: it assembles it (tools, local_roots policy,
// runners), starts the link and blocks until ctx is cancelled (SIGINT or
// SIGTERM, or the service manager's stop request). A startup failure, such as
// a missing tool, is returned and ends the process with a non-zero exit code.
//
// When the process was started by a service manager (systemd, the Windows
// Service Control Manager) the work runs inside service.Host, which answers
// the manager's protocol; interactively it runs directly.
func runCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "run")
	configPath := fs.String("config", "", "path to the YAML config file (default $"+config.PathEnv+"; optional)")
	logFile := fs.String("log-file", "", "append log lines to this file instead of stderr")
	hideConsole := fs.Bool("hide-console", false, "Windows: close the console window (needs --log-file); used by the per-user scheduled task")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	path := *configPath
	if path == "" {
		path, _ = e.lookupEnv(config.PathEnv)
	}
	logOut := e.stderr
	if *logFile != "" {
		f, err := openLogFile(*logFile)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		logOut = f
	}
	if *hideConsole {
		if *logFile == "" {
			return errors.New("--hide-console needs --log-file: without a console there is nowhere else to log")
		}
		if err := service.HideConsole(); err != nil {
			return err
		}
	}

	// A worker that logs to a file (the Windows user task, whose console
	// is hidden) also logs the error it stops with there: run prints it on
	// stderr, which nobody sees then.
	logFailure := *logFile != ""
	work := func(ctx context.Context, log io.Writer) error { return runWorker(ctx, e, path, log, logFailure) }
	if host := e.serviceHost(); host.Managed() {
		return host.Run(ctx, service.Name, logOut, work)
	}
	return work(ctx, logOut)
}

// errBrokenRelease is how a build with brokenRelease (the upgrade test's
// broken release) fails once it started.
var errBrokenRelease = errors.New("this build is the broken release of the upgrade test (-tags syncr_broken_release): it does not run")

// dataDirLockWait is how long `run` waits for the data directory's lock
// while another process holds it (datadir.Lock): long enough for the
// process of a worker that its service manager has just stopped to exit,
// short enough that a second worker on the same directory is refused
// soon.
const dataDirLockWait = 20 * time.Second

// maxLogFileBytes is the size at which --log-file is rotated at startup.
const maxLogFileBytes = 10 << 20

// openLogFile opens path for appending (mode 0600). A file larger than
// maxLogFileBytes is first moved to path+".1", replacing the previous one.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create the log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogFileBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the operator's --log-file
	if err != nil {
		return nil, fmt.Errorf("open the log file: %w", err)
	}
	return f, nil
}

// runWorker loads the configuration, takes the data directory, builds the
// worker, starts its housekeeping (startBackground) and the link and waits
// for ctx. With logFailure, the error it returns is also logged to logOut
// (with the configured logger once it exists, a plain one before).
//
// Before anything else uses the data directory it takes its lock
// (datadir.Lock: a second worker on the same directory is refused, as two
// would remove each other's job files), released once the worker stopped.
// While another process holds it, it waits up to dataDirLockWait and says
// so: a service manager may start it while the worker it replaces is
// still exiting. Holding it, it removes what jobs of an earlier process
// left in the scratch directories (that process is gone, so nothing uses
// them), and what an earlier upgrade left next to the executable
// (upgrade.RemoveLeftover, which never removes a backup an upgrade in
// progress may still need). Neither cleanup is fatal. run never prunes
// tool versions: a worker rolled back by an upgrade still needs its own.
func runWorker(ctx context.Context, e *env, configPath string, logOut io.Writer, logFailure bool) (err error) {
	logger := slog.New(slog.NewTextHandler(logOut, nil))
	if logFailure {
		defer func() {
			if err != nil {
				logger.Error("the worker stopped with an error", "error", err.Error())
			}
		}()
	}
	cfg := workerconfig.Default()
	if err := config.Load(configPath, e.lookupEnv, &cfg); err != nil {
		return err
	}
	configured, err := logging.New(logOut, cfg.Log)
	if err != nil {
		return err
	}
	logger = configured

	logger.Info("worker starting",
		"version", e.info.Version, "commit", e.info.Commit, "os", runtime.GOOS, "arch", runtime.GOARCH)
	dataDir, err := resolveDataDir(cfg, e.pathsEnv())
	if err != nil {
		return err
	}
	wait := cmp.Or(e.lockWait, dataDirLockWait)
	release, err := datadir.Lock(ctx, dataDir, wait, func(lockFile string) {
		logger.Warn("another process holds the data directory's lock; waiting for it to exit (a worker that is still stopping "+
			"lets it go in a moment, a second worker on this data directory does not)", "lock_file", lockFile, "wait", wait.String())
	})
	if err != nil {
		if ctx.Err() != nil {
			logger.Info("stopped while waiting for the data directory's lock")
			return nil
		}
		return err
	}
	defer func() {
		if err := release(); err != nil {
			logger.Warn("releasing the data directory lock failed", "error", err.Error())
		}
	}()
	cleanStart(dataDir, logger)
	w, err := buildWorker(ctx, cfg, workerDeps{
		paths:      e.pathsEnv(),
		bundledDir: executableDir(),
		tools:      e.tools,
	}, logger)
	if err != nil {
		return err
	}
	logger.Info("worker ready", "job_kinds", w.registry.Kinds())
	stopBackground := w.startBackground(ctx, logger)
	defer stopBackground()

	stop, err := e.linkStarter()(ctx, cfg, w, logger)
	if err != nil {
		return err
	}
	if brokenRelease {
		stop()
		return errBrokenRelease
	}
	<-ctx.Done()
	stop()
	logger.Info("worker stopped")
	return nil
}

// cleanStart removes the files that jobs of an earlier worker process left
// in the scratch directories of dataDir, and the leftovers of an earlier
// upgrade next to the executable that no upgrade can still need
// (upgrade.RemoveLeftover). Failures are logged; the worker starts anyway.
func cleanStart(dataDir string, log *slog.Logger) {
	scratch := paths.ScratchDir(dataDir)
	if err := datadir.RemoveContents(filepath.Join(scratch, "rclone"), filepath.Join(scratch, "restic"), filepath.Join(scratch, "mount")); err != nil {
		log.Warn("could not remove all files earlier jobs left in the scratch directory; they stay until the next start",
			"scratch_dir", scratch, "error", err.Error())
	}
	exe, err := executablePath()
	if err != nil {
		return
	}
	if err := upgrade.RemoveLeftover(exe); err != nil {
		log.Info("what an earlier upgrade left next to the worker binary could not be removed yet; the next start tries again", "error", err.Error())
	}
}

// versionCmd prints build information.
func versionCmd(_ context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "version")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	_, err := fmt.Fprintf(e.stdout, "%s %s %s/%s\n", name, e.info, runtime.GOOS, runtime.GOARCH)
	return err
}
