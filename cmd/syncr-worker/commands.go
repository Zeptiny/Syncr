package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
)

// runCmd starts the worker and blocks until ctx is cancelled (SIGINT or
// SIGTERM). Milestone 2 starts the link transport and job agent here.
func runCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "run")
	configPath := fs.String("config", "", "path to the YAML config file (default $"+config.PathEnv+"; optional)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	path := *configPath
	if path == "" {
		path, _ = e.lookupEnv(config.PathEnv)
	}
	cfg := workerconfig.Default()
	if err := config.Load(path, e.lookupEnv, &cfg); err != nil {
		return err
	}
	logger, err := logging.New(e.stderr, cfg.Log)
	if err != nil {
		return err
	}

	logger.Info("worker started",
		"version", e.info.Version, "commit", e.info.Commit, "os", runtime.GOOS, "arch", runtime.GOARCH)
	logger.Warn("worker link not implemented yet; idling until stopped")
	<-ctx.Done()
	logger.Info("worker stopped")
	return nil
}

// errNotImplemented is returned by commands that later milestones deliver.
var errNotImplemented = errors.New("not implemented yet")

// setupCmd will enroll the worker, write its config, provision rclone and
// restic, and install the service (Milestone 2).
func setupCmd(_ context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "setup")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	return errNotImplemented
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
