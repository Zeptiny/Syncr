package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/service"
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

	work := func(ctx context.Context, log io.Writer) error { return runWorker(ctx, e, path, log) }
	if host := e.serviceHost(); host.Managed() {
		return host.Run(ctx, service.Name, logOut, work)
	}
	return work(ctx, logOut)
}

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

// runWorker loads the configuration, builds the worker, starts the link and
// waits for ctx.
func runWorker(ctx context.Context, e *env, configPath string, logOut io.Writer) error {
	cfg := workerconfig.Default()
	if err := config.Load(configPath, e.lookupEnv, &cfg); err != nil {
		return err
	}
	logger, err := logging.New(logOut, cfg.Log)
	if err != nil {
		return err
	}

	logger.Info("worker starting",
		"version", e.info.Version, "commit", e.info.Commit, "os", runtime.GOOS, "arch", runtime.GOARCH)
	w, err := buildWorker(ctx, cfg, workerDeps{
		paths:      e.pathsEnv(),
		bundledDir: executableDir(),
		tools:      e.tools,
	}, logger)
	if err != nil {
		return err
	}
	logger.Info("worker ready", "job_kinds", w.registry.Kinds())

	stop, err := e.linkStarter()(ctx, cfg, w, logger)
	if err != nil {
		return err
	}
	<-ctx.Done()
	stop()
	logger.Info("worker stopped")
	return nil
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
