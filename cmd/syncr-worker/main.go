// Command syncr-worker runs jobs for a Syncr controller by supervising rclone
// and restic child processes.
//
// This package is the composition root of the worker: it is the only place
// that reads the process environment, builds runners, transports and the
// agent, and wires them together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/version"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
)

const name = "syncr-worker"

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], &env{
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		lookupEnv: os.LookupEnv,
		info:      version.Get(),
	})
	stop()
	os.Exit(code)
}

// env carries the process-level dependencies of every command.
type env struct {
	stdout, stderr io.Writer
	lookupEnv      config.LookupEnv
	info           version.Info
	tools          toolsOverrides // zero value: pinned tools, real network
	// host connects `run` to the service manager; nil means the real one.
	host service.Host
	// link starts the link transport inside `run`; nil means startLink.
	link linkStarter
	// setup replaces the collaborators of `setup` and `service`; the zero
	// value means the real service manager, controller and network.
	setup setupOverrides
}

func (e *env) serviceHost() service.Host {
	if e.host != nil {
		return e.host
	}
	return service.NewHost()
}

// pathsEnv returns the environment for locating the default data directory.
func (e *env) pathsEnv() paths.Env {
	return paths.Env{GOOS: runtime.GOOS, LookupEnv: e.lookupEnv, UserHomeDir: os.UserHomeDir, Geteuid: os.Geteuid}
}

// command is one subcommand. Adding a subcommand means adding an entry to
// commands; each command parses its own flags.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, e *env, args []string) error
}

func commands() []command {
	return []command{
		{name: "run", summary: "run the worker until interrupted", run: runCmd},
		{name: "setup", summary: "enroll this machine and install the worker service", run: setupCmd},
		{name: "service", summary: "install, remove, start, stop or inspect the worker service", run: serviceCmd},
		{name: "tools", summary: "provision and inspect the pinned rclone and restic binaries", run: toolsCmd},
		{name: "version", summary: "print version information", run: versionCmd},
	}
}

// errUsage reports invalid arguments whose explanation was already printed.
var errUsage = errors.New("usage")

// run dispatches args to a subcommand and returns the process exit code.
func run(ctx context.Context, args []string, e *env) int {
	if len(args) == 0 || isHelp(args[0]) {
		usage(e.stderr)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		err := c.run(ctx, e, args[1:])
		switch {
		case err == nil:
			return exitOK
		case errors.Is(err, errUsage):
			return exitUsage
		default:
			fmt.Fprintf(e.stderr, "%s %s: %v\n", name, c.name, err)
			return exitError
		}
	}
	fmt.Fprintf(e.stderr, "%s: unknown command %q\n\n", name, args[0])
	usage(e.stderr)
	return exitUsage
}

func isHelp(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "-help" || arg == "--help"
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", name)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nRun '%s <command> -h' for command flags.\n", name)
}

// newFlagSet returns a flag set for a subcommand that reports errors on
// stderr instead of exiting.
func newFlagSet(e *env, cmd string) *flag.FlagSet {
	fs := flag.NewFlagSet(name+" "+cmd, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

// parseFlags parses args, mapping -h to a clean exit and other parse errors
// (already printed by flag) to errUsage.
func parseFlags(fs *flag.FlagSet, args []string) (helped bool, err error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected arguments: %q\n", fs.Args())
		return false, errUsage
	}
	return false, nil
}
