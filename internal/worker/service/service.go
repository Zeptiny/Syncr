// Package service installs, controls and hosts the worker as an operating
// system service.
//
// Platform behavior:
//
//   - Linux, system scope (root): a systemd unit in /etc/systemd/system.
//   - Linux, user scope: a systemd user unit in ~/.config/systemd/user. It
//     starts at login; "loginctl enable-linger <user>" keeps it running
//     without a login session.
//   - Windows, system scope (Administrator): a Windows Service in the Service
//     Control Manager, running as LocalSystem, logging to the Event Log.
//   - Windows, user scope: a per-user Scheduled Task that starts at logon, run
//     with the user's own rights (no administrator needed). It is a task and
//     not a service because only administrators can create services.
//
// The installers are built on github.com/kardianos/service behind the small
// Manager interface, so callers and tests never touch the operating system.
// Host is the other half: it lets "syncr-worker run" speak to the service
// manager when it was started by one.
//
// Allowed dependencies: the standard library, kardianos/service and
// golang.org/x/sys.
package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Name is the service name on every platform.
const Name = "syncr-worker"

// Scope selects between a machine-wide and a per-user installation.
type Scope int

// The scopes.
const (
	// ScopeSystem installs for the whole machine. It needs root or
	// Administrator rights.
	ScopeSystem Scope = iota
	// ScopeUser installs for the current user only and needs no elevated
	// rights.
	ScopeUser
)

// String implements fmt.Stringer.
func (s Scope) String() string {
	if s == ScopeUser {
		return "user"
	}
	return "system"
}

// State is the state of the installed service.
type State int

// The states.
const (
	// StateNotInstalled means the service does not exist.
	StateNotInstalled State = iota
	// StateStopped means the service is installed and not running.
	StateStopped
	// StateRunning means the service is running.
	StateRunning
	// StateFailing means the service is in a failed state or in a restart
	// loop: it starts and exits again.
	StateFailing
)

// String implements fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateNotInstalled:
		return "not installed"
	case StateStopped:
		return "stopped"
	case StateRunning:
		return "running"
	case StateFailing:
		return "failing (crashed or restarting)"
	default:
		return "unknown"
	}
}

// Spec describes the service to install.
type Spec struct {
	// Name is the service name; use Name.
	Name        string
	DisplayName string
	Description string
	// Executable is the absolute path of the program to run.
	Executable string
	// Args are its arguments, e.g. {"run", "--config", "/etc/syncr/worker.yaml"}.
	Args []string
	// WorkDir is the working directory (ignored on Windows).
	WorkDir string
	// LogFile, if set, is where a Windows user task writes the worker log
	// (it has no journal or Event Log).
	LogFile string
	Scope   Scope
}

// NewSpec returns the Spec for the worker: the executable run with
// "run --config configPath". On Windows in user scope the worker also logs to
// logFile and hides its console window.
func NewSpec(goos string, scope Scope, executable, configPath, workDir, logFile string) Spec {
	args := []string{"run", "--config", configPath}
	spec := Spec{
		Name:        Name,
		DisplayName: "Syncr Worker",
		Description: "Runs Syncr rclone and restic jobs for a Syncr controller.",
		Executable:  executable,
		WorkDir:     workDir,
		Scope:       scope,
	}
	if goos == "windows" && scope == ScopeUser {
		args = append(args, "--log-file", logFile, "--hide-console")
		spec.LogFile = logFile
	}
	spec.Args = args
	return spec
}

// Validate reports a Spec that cannot be installed on this system.
func (s Spec) Validate() error { return s.validate(runtime.GOOS) }

func (s Spec) validate(goos string) error {
	switch {
	case s.Name == "" || strings.ContainsAny(s.Name, " \t/\\"):
		return fmt.Errorf("service: invalid name %q", s.Name)
	case !isAbs(goos, s.Executable):
		return fmt.Errorf("service: executable %q is not an absolute path", s.Executable)
	case s.WorkDir != "" && !isAbs(goos, s.WorkDir):
		return fmt.Errorf("service: working directory %q is not an absolute path", s.WorkDir)
	}
	// The service manager starts the worker in a directory of its own
	// choosing (WorkDir, or System32 for the Windows SCM), so a relative path
	// would name a different file there than it did for the installer.
	for i, arg := range s.Args {
		if (arg == "--config" || arg == "--log-file") && i+1 < len(s.Args) && !isAbs(goos, s.Args[i+1]) {
			return fmt.Errorf("service: %s %q is not an absolute path", arg, s.Args[i+1])
		}
	}
	return nil
}

// isAbs is filepath.IsAbs for the given operating system, so the rules of
// another system can be checked (and tested) anywhere.
func isAbs(goos, p string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// Info describes where a service lives and how to see its logs; it is used to
// print actionable messages.
type Info struct {
	// Platform names the service manager, e.g. "systemd" or "Windows Service".
	Platform string
	// Location is the unit file or task name, when there is one.
	Location string
	// LogHint is a command or place to read the service's logs.
	LogHint string
	// Notes are extra remarks to show after installing.
	Notes []string
}

// Manager installs and controls the service. Methods other than Info return
// ErrNotInstalled (wrapped) when the service does not exist, except Status,
// which reports StateNotInstalled.
type Manager interface {
	// Preflight reports, before anything is changed, whether spec can be
	// installed with the current rights. The error is a *PermissionError when
	// elevation is missing.
	Preflight(spec Spec) error
	// Install creates the service and enables it at boot (or logon). It fails
	// if the service already exists.
	Install(ctx context.Context, spec Spec) error
	// Uninstall stops and removes the service.
	Uninstall(ctx context.Context, spec Spec) error
	Start(ctx context.Context, spec Spec) error
	Stop(ctx context.Context, spec Spec) error
	Restart(ctx context.Context, spec Spec) error
	Status(ctx context.Context, spec Spec) (State, error)
	// Info describes spec's service on this platform.
	Info(spec Spec) Info
}

// Errors returned by Manager implementations.
var (
	// ErrNotInstalled means the service does not exist.
	ErrNotInstalled = errors.New("service is not installed")
	// ErrAlreadyInstalled means Install found the service in place.
	ErrAlreadyInstalled = errors.New("service is already installed")
	// ErrUnsupported means this platform or init system cannot host the
	// requested kind of service.
	ErrUnsupported = errors.New("service management is not supported here")
)

// PermissionError reports that installing or controlling the service needs
// rights the process lacks. Its message says what to do.
type PermissionError struct {
	Scope Scope
	// Hint says how to fix it.
	Hint string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("installing a %s service needs elevated rights: %s", e.Scope, e.Hint)
}
