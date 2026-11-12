package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/config"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/upgrade"
)

// upgradeOverrides lets tests replace what `upgrade` downloads, runs and
// changes; every nil or zero field selects the real one (see
// upgrade.Config). The service manager is setupOverrides.services and the
// binary to replace setupOverrides.executable, as for `service`.
type upgradeOverrides struct {
	http         upgrade.HTTPDoer
	binary       upgrade.Binary
	swapper      upgrade.Swapper
	pruner       upgrade.Pruner
	sleep        func(ctx context.Context, d time.Duration) error
	healthWindow time.Duration
}

// upgradeUsage is the help of `upgrade`; the flags follow it.
const upgradeUsage = `Usage: %[1]s upgrade [--version vX.Y.Z] [--from URL] [--user] [--config PATH] [--no-restart] [--dry-run]

Replaces this worker's binary with another release and restarts its service,
in a way that cannot leave the worker stopped:
  1. download checksums.txt and %[1]s_<os>_<arch>.tar.gz (.zip on Windows)
     and require the archive's SHA-256 to match (nothing skips this)
  2. run the new binary's "version" (it must report --version, if given)
  3. fetch the rclone and restic versions it pins into the worker's tools
     directory, and check that it finds them with this worker's config
  4. swap the binaries (the old one is kept until the upgrade succeeded)
  5. restart the service and require it to stay running for 20 seconds
  6. on failure, put the old binary back and start it again; on success,
     remove the tool versions no longer used
Nothing on the machine changes before step 4, and only one upgrade of a worker
runs at a time. Running jobs are interrupted by the restart, as with any
restart of the worker. Run it as the user that installed the worker: root
(sudo) or an elevated terminal for the system service, yourself with --user.
It keeps running if the terminal goes away (an SSH connection that drops);
inside tmux or screen you can reconnect to see how it ended. A worker in a
container is upgraded by pulling the new image instead.

Flags:
`

// upgradeExitHelp ends the help of `upgrade`.
const upgradeExitHelp = `
Exit status: 0 when the worker runs the new release (or already ran it, or a
dry run checked it out); 1 when the upgrade was refused or failed (the message
says whether anything changed, and where the service's log is); 2 for invalid
arguments.
`

// upgradeCmd upgrades the installed worker in place (internal/worker/upgrade):
// the binary this command runs from, the service of --user's scope, the tools
// directory and tool paths of --config.
func upgradeCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "upgrade")
	version := fs.String("version", "", "the release to install, e.g. v2.1.0 (default: the latest release)")
	from := fs.String("from", "", "base URL of the release's files on a mirror; https, or http to this machine only\n(default: GitHub's release of --version, or the latest release)")
	user := fs.Bool("user", false, "upgrade the per-user service (set up with --user) instead of the machine-wide one")
	configPath := fs.String("config", "", "the worker's config file (default: the platform default for the scope)")
	noRestart := fs.Bool("no-restart", false, "replace the binary but restart nothing, for a worker set up with --no-service;\nrestart it yourself afterwards")
	dryRun := fs.Bool("dry-run", false, "download and verify the release, print what would be done, and change nothing")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), upgradeUsage, name)
		fs.PrintDefaults()
		fmt.Fprint(fs.Output(), upgradeExitHelp)
	}
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	ignoreHangup()

	scope, pathsMode := service.ScopeSystem, paths.ModeSystem
	if *user {
		scope, pathsMode = service.ScopeUser, paths.ModeUser
	}
	pe := e.pathsEnv()
	cfgPath := *configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = paths.ConfigFile(pathsMode, pe); err != nil {
			return fmt.Errorf("find the worker's config file (use --config): %w", err)
		}
	}
	cfgPath, err := filepath.Abs(cfgPath)
	if err != nil {
		return fmt.Errorf("--config %q: %w", cfgPath, err)
	}
	if _, err := os.Stat(cfgPath); err != nil { //nolint:gosec,nolintlint // the operator's --config path; gosec flags it only on some GOOS
		return fmt.Errorf("the worker's config file %s is not readable (%w): pass --config with the file its service runs with%s",
			cfgPath, err, scopeHint(*user))
	}
	cfg := workerconfig.Default()
	if err := config.Load(cfgPath, e.lookupEnv, &cfg); err != nil {
		return err
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		if dataDir, err = paths.DataDir(pathsMode, pe); err != nil {
			return fmt.Errorf("find the worker data directory (set data_dir in %s): %w", cfgPath, err)
		}
	}
	toolsDir := cfg.Tools.Dir
	if toolsDir == "" {
		toolsDir = paths.ToolsDir(dataDir)
	}
	exe, err := e.setup.exe()
	if err != nil {
		return fmt.Errorf("find the worker executable: %w", err)
	}

	o := e.upgrade
	res, err := upgrade.Run(ctx, upgrade.Config{
		Version:    *version,
		From:       *from,
		Executable: exe,
		Current:    e.info.Version,
		ToolsDir:   toolsDir,
		ToolPaths:  map[string]string{toolRclone: cfg.Tools.RclonePath, toolRestic: cfg.Tools.ResticPath},
		Service: service.NewSpec(e.setup.goosOrDefault(), scope, exe, cfgPath, dataDir,
			filepath.Join(dataDir, "logs", "worker.log")),
		NoRestart:    *noRestart,
		DryRun:       *dryRun,
		HealthWindow: o.healthWindow,
		Out:          e.stdout,
		HTTP:         o.http,
		Services:     e.setup.manager(),
		Binary:       o.binary,
		Swapper:      o.swapper,
		Pruner:       o.pruner,
		Sleep:        o.sleep,
	})
	var perm *service.PermissionError
	switch {
	case errors.Is(err, upgrade.ErrInvalidVersion), errors.Is(err, upgrade.ErrInsecureSource):
		fmt.Fprintf(e.stderr, "%s upgrade: %v\n", name, err)
		return errUsage
	case errors.As(err, &perm) && !errors.Is(err, upgrade.ErrRolledBack) && !errors.Is(err, upgrade.ErrRollbackFailed):
		// Refused before the swap. After it, the rollback's own message
		// says what changed and how to recover, so it is kept as it is.
		return fmt.Errorf("upgrading the worker of the %s service needs elevated rights: %s; nothing was changed", scope, perm.Hint)
	case err != nil:
		return err
	}
	if res.Restarted || (res.Swapped && *noRestart) {
		fmt.Fprintf(e.stdout, "The worker binary %s is now %s (was %s).\n", exe, res.New, res.Old)
	}
	return nil
}

// scopeHint suggests --user when it was not given.
func scopeHint(user bool) string {
	if user {
		return ""
	}
	return " (a worker set up with --user needs --user here too)"
}
