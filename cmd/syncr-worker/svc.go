package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
)

// serviceCmd dispatches `service install|uninstall|start|stop|restart|status`.
func serviceCmd(ctx context.Context, e *env, args []string) error {
	actions := []struct{ name, summary string }{
		{"install", "install the service (add --start to start it)"},
		{"uninstall", "stop and remove the service"},
		{"start", "start the service"},
		{"stop", "stop the service"},
		{"restart", "restart the service"},
		{"status", "show whether the service runs (exit status 1 if it does not)"},
	}
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Fprintf(e.stderr, "Usage: %s service <action> [--user] [--config PATH]\n\nActions:\n", name)
		for _, a := range actions {
			fmt.Fprintf(e.stderr, "  %-10s %s\n", a.name, a.summary)
		}
		fmt.Fprintf(e.stderr, "\nWithout --user the machine-wide service is meant, which needs root or Administrator.\n")
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	action := args[0]
	known := false
	for _, a := range actions {
		known = known || a.name == action
	}
	if !known {
		fmt.Fprintf(e.stderr, "unknown service action %q\n", action)
		return errUsage
	}

	fs := newFlagSet(e, "service "+action)
	user := fs.Bool("user", false, "the per-user service instead of the machine-wide one")
	configPath := fs.String("config", "", "worker config file (default: the platform default for the scope)")
	dataDir := fs.String("data-dir", "", "worker data directory (default: platform default, $"+dataDirEnv+")")
	start := fs.Bool("start", false, "install: also start the service")
	if helped, err := parseFlags(fs, args[1:]); helped || err != nil {
		return err
	}
	if *start && action != "install" {
		fmt.Fprintf(fs.Output(), "--start only applies to install\n")
		return errUsage
	}

	scope, pathsMode := service.ScopeSystem, paths.ModeSystem
	if *user {
		scope, pathsMode = service.ScopeUser, paths.ModeUser
	}
	pe := e.pathsEnv()
	cfgPath := *configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = paths.ConfigFile(pathsMode, pe); err != nil {
			return fmt.Errorf("find the config file (use --config): %w", err)
		}
	}
	dir := *dataDir
	if dir == "" {
		dir, _ = e.lookupEnv(dataDirEnv)
	}
	if dir == "" {
		var err error
		if dir, err = paths.DataDir(pathsMode, pe); err != nil {
			return fmt.Errorf("find the data directory (use --data-dir): %w", err)
		}
	}
	exe, err := e.setup.exe()
	if err != nil {
		return fmt.Errorf("find the worker executable: %w", err)
	}
	spec := service.NewSpec(e.setup.goosOrDefault(), scope, exe, cfgPath, dir, filepath.Join(dir, "logs", "worker.log"))
	mgr := e.setup.manager()
	info := mgr.Info(spec)

	switch action {
	case "install":
		if _, err := os.Stat(cfgPath); err != nil { //nolint:gosec // the operator's --config path
			return fmt.Errorf("the config file %s is not readable (%w): run '%s setup' first, or point --config at it", cfgPath, err, name)
		}
		if err := mgr.Install(ctx, spec); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "installed %s (%s): %s\n", spec.Name, info.Platform, info.Location)
		for _, n := range info.Notes {
			fmt.Fprintf(e.stdout, "note: %s\n", n)
		}
		if *start {
			if err := mgr.Start(ctx, spec); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "started %s\n", spec.Name)
		} else {
			fmt.Fprintf(e.stdout, "start it with: %s service start%s\n", name, scopeSuffix(*user))
		}
	case "uninstall":
		if err := mgr.Uninstall(ctx, spec); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "removed %s. The config and data were left in place (%s, %s).\n", spec.Name, cfgPath, dir)
	case "start", "stop", "restart":
		var err error
		switch action {
		case "start":
			err = mgr.Start(ctx, spec)
		case "stop":
			err = mgr.Stop(ctx, spec)
		default:
			err = mgr.Restart(ctx, spec)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "%s: ok\n", action)
	case "status":
		st, err := mgr.Status(ctx, spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "%s: %s (%s)\n", spec.Name, st, info.Platform)
		if st != service.StateRunning {
			if info.LogHint != "" {
				fmt.Fprintf(e.stdout, "logs: %s\n", info.LogHint)
			}
			return errors.New("the service is " + st.String())
		}
	}
	return nil
}

func scopeSuffix(user bool) string {
	if user {
		return " --user"
	}
	return ""
}
