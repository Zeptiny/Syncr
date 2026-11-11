package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	kservice "github.com/kardianos/service"
)

// serviceFactory builds the kardianos service for a configuration.
type serviceFactory func(i kservice.Interface, c *kservice.Config) (kservice.Service, error)

// deps are the manager's inputs that tests replace.
type deps struct {
	goos       string
	elevated   func() bool
	lookupEnv  func(string) (string, bool)
	platform   func() string
	newService serviceFactory
	runner     commandRunner
	// taskUser is the account a Windows user task runs as ("DOMAIN\name").
	taskUser func() (string, error)
	tempDir  func() string
	// serviceCommandLine reads a Windows Service's command line from the
	// Service Control Manager.
	serviceCommandLine func(name string) (string, error)
	// sleep waits d or until ctx ends (polling a task while it stops).
	sleep func(ctx context.Context, d time.Duration) error
}

// NewManager returns the Manager for the operating system the program runs
// on.
func NewManager() Manager {
	return newManager(deps{
		goos:       runtime.GOOS,
		elevated:   Elevated,
		lookupEnv:  os.LookupEnv,
		platform:   kservice.Platform,
		newService: kservice.New,
		runner:     execRunner{},
		taskUser:   currentUser,
		tempDir:    os.TempDir,

		serviceCommandLine: serviceCommandLine,
		sleep:              sleepCtx,
	})
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

func currentUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("find the current user: %w", err)
	}
	return u.Username, nil
}

type manager struct {
	deps
	tasks *taskManager
}

func newManager(d deps) *manager {
	return &manager{deps: d, tasks: &taskManager{runner: d.runner, user: d.taskUser, tempDir: d.tempDir, sleep: d.sleep}}
}

// noopProgram satisfies kservice.Interface for control operations, which
// never run the program.
type noopProgram struct{}

func (noopProgram) Start(kservice.Service) error { return nil }
func (noopProgram) Stop(kservice.Service) error  { return nil }

// useTask reports whether spec is a Windows per-user scheduled task.
func (m *manager) useTask(spec Spec) bool { return m.goos == "windows" && spec.Scope == ScopeUser }

func (m *manager) isSystemd() bool { return m.platform() == "linux-systemd" }

// Preflight implements Manager.
func (m *manager) Preflight(spec Spec) error {
	if err := spec.validate(m.goos); err != nil {
		return err
	}
	if m.useTask(spec) {
		return nil
	}
	if spec.Scope == ScopeSystem {
		if !m.elevated() {
			return &PermissionError{Scope: ScopeSystem, Hint: elevationHint(m.goos)}
		}
		return nil
	}
	if m.goos != "linux" {
		return nil
	}
	if !m.isSystemd() {
		return fmt.Errorf("%w: user services need systemd (found %q); start \"syncr-worker run\" from your session instead", ErrUnsupported, m.platform())
	}
	if v, ok := m.lookupEnv("XDG_RUNTIME_DIR"); !ok || v == "" {
		return errors.New("the user systemd instance is not reachable (XDG_RUNTIME_DIR is not set): " +
			"log in to a session on this machine (not through plain su or sudo), or export XDG_RUNTIME_DIR=/run/user/$(id -u)")
	}
	return nil
}

func elevationHint(goos string) string {
	if goos == "windows" {
		return "run this command from an elevated terminal (right-click, Run as administrator), or use --user to install a per-user task that needs no administrator rights"
	}
	return "run it as root (for example with sudo), or use --user to install a per-user service that needs no root"
}

func (m *manager) config(spec Spec) *kservice.Config {
	cfg := &kservice.Config{
		Name:             spec.Name,
		DisplayName:      spec.DisplayName,
		Description:      spec.Description,
		Executable:       spec.Executable,
		Arguments:        spec.Args,
		WorkingDirectory: spec.WorkDir,
		Option:           kservice.KeyValue{},
	}
	switch m.goos {
	case "windows":
		cfg.WorkingDirectory = "" // not supported by the SCM
		// The option names are Windows-only constants in kardianos/service,
		// which are not visible when building for other systems.
		cfg.Option["StartType"] = "automatic"
		cfg.Option["DelayedAutoStart"] = true
		cfg.Option["OnFailure"] = "restart"
		cfg.Option["OnFailureDelayDuration"] = "10s"
		cfg.Option["OnFailureResetPeriod"] = 24 * 60 * 60
	default:
		cfg.Option["SystemdScript"] = unitTemplate(spec.Scope)
		cfg.Option["UserService"] = spec.Scope == ScopeUser
	}
	return cfg
}

func (m *manager) service(spec Spec) (kservice.Service, error) {
	svc, err := m.newService(noopProgram{}, m.config(spec))
	if err != nil {
		if errors.Is(err, kservice.ErrNoServiceSystemDetected) {
			return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
		return nil, fmt.Errorf("prepare the service: %w", err)
	}
	return svc, nil
}

// Status implements Manager.
func (m *manager) Status(ctx context.Context, spec Spec) (State, error) {
	if err := spec.validate(m.goos); err != nil {
		return StateNotInstalled, err
	}
	if m.useTask(spec) {
		return m.tasks.status(ctx, spec)
	}
	if m.goos == "linux" && m.isSystemd() {
		return systemdState(ctx, m.runner, spec)
	}
	svc, err := m.service(spec)
	if err != nil {
		return StateNotInstalled, err
	}
	st, err := svc.Status()
	switch {
	case errors.Is(err, kservice.ErrNotInstalled):
		return StateNotInstalled, nil
	case err != nil:
		return StateFailing, fmt.Errorf("query the service: %w", err)
	case st == kservice.StatusRunning:
		return StateRunning, nil
	default:
		return StateStopped, nil
	}
}

// Install implements Manager.
func (m *manager) Install(ctx context.Context, spec Spec) error {
	if err := m.Preflight(spec); err != nil {
		return err
	}
	st, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if st != StateNotInstalled {
		return fmt.Errorf("%w (%s, currently %s); uninstall it first or use setup --force", ErrAlreadyInstalled, spec.Name, st)
	}
	if m.useTask(spec) {
		return m.tasks.install(ctx, spec)
	}
	svc, err := m.service(spec)
	if err != nil {
		return err
	}
	if err := svc.Install(); err != nil {
		return m.wrap("install", err)
	}
	return nil
}

// Uninstall implements Manager.
func (m *manager) Uninstall(ctx context.Context, spec Spec) error {
	if err := m.Preflight(spec); err != nil {
		return err
	}
	st, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if st == StateNotInstalled {
		return fmt.Errorf("%s: %w", spec.Name, ErrNotInstalled)
	}
	if m.useTask(spec) {
		return m.tasks.uninstall(ctx, spec)
	}
	svc, err := m.service(spec)
	if err != nil {
		return err
	}
	if st != StateStopped {
		_ = svc.Stop() // a failing stop must not keep the service installed
	}
	if err := svc.Uninstall(); err != nil {
		return m.wrap("uninstall", err)
	}
	return nil
}

// control runs a start, stop or restart.
func (m *manager) control(ctx context.Context, spec Spec, action string) error {
	if err := m.Preflight(spec); err != nil {
		return err
	}
	st, err := m.Status(ctx, spec)
	if err != nil {
		return err
	}
	if st == StateNotInstalled {
		return fmt.Errorf("%s: %w", spec.Name, ErrNotInstalled)
	}
	// A stopped service is not stopped again, and restarting it starts it.
	// The Windows Service Control Manager refuses to stop a service that is
	// not running, so a stop would fail and kardianos' Restart (stop, then
	// start) too; ending a task that does not run fails with a message in
	// the display language. systemd's restart starts a stopped unit anyway.
	switch {
	case action == "stop" && st == StateStopped:
		return nil
	case action == "restart" && st == StateStopped:
		action = "start"
	}
	if m.useTask(spec) {
		return m.tasks.control(ctx, spec, action)
	}
	svc, err := m.service(spec)
	if err != nil {
		return err
	}
	switch action {
	case "start":
		err = svc.Start()
	case "stop":
		err = svc.Stop()
	default:
		err = svc.Restart()
	}
	if err != nil {
		return m.wrap(action, err)
	}
	return nil
}

// Start implements Manager.
func (m *manager) Start(ctx context.Context, spec Spec) error { return m.control(ctx, spec, "start") }

// Stop implements Manager.
func (m *manager) Stop(ctx context.Context, spec Spec) error { return m.control(ctx, spec, "stop") }

// Restart implements Manager.
func (m *manager) Restart(ctx context.Context, spec Spec) error {
	return m.control(ctx, spec, "restart")
}

// wrap adds the operation to an error and turns permission failures into a
// *PermissionError with a fix.
func (m *manager) wrap(action string, err error) error {
	if errors.Is(err, fs.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "access is denied") {
		return &PermissionError{Scope: ScopeSystem, Hint: elevationHint(m.goos)}
	}
	return fmt.Errorf("%s the service: %w", action, err)
}

// Info implements Manager.
func (m *manager) Info(spec Spec) Info {
	switch {
	case m.useTask(spec):
		return Info{
			Platform: "Windows Scheduled Task (at logon)",
			Location: `Task Scheduler, task "` + spec.Name + `"`,
			LogHint:  "Get-Content -Wait '" + spec.LogFile + "'",
			Notes: []string{
				"The worker starts when this user logs on and stops when the task is ended; it does not run while nobody is logged on.",
			},
		}
	case m.goos == "windows":
		return Info{
			Platform: "Windows Service (LocalSystem)",
			Location: `Services, "` + spec.Name + `"`,
			LogHint:  "Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='" + spec.Name + "'} -MaxEvents 30",
		}
	case spec.Scope == ScopeUser:
		home, _ := m.lookupEnv("HOME")
		return Info{
			Platform: "systemd user unit",
			Location: home + "/.config/systemd/user/" + spec.Name + ".service",
			LogHint:  "journalctl --user -u " + spec.Name + " -f",
			Notes: []string{
				"A user service runs while you are logged in. To keep it running after logout and start it at boot, run once: sudo loginctl enable-linger " + userName(m.lookupEnv),
			},
		}
	default:
		return Info{
			Platform: "systemd system unit",
			Location: "/etc/systemd/system/" + spec.Name + ".service",
			LogHint:  "journalctl -u " + spec.Name + " -f",
		}
	}
}

func userName(lookup func(string) (string, bool)) string {
	for _, k := range []string{"USER", "LOGNAME"} {
		if v, ok := lookup(k); ok && v != "" {
			return v
		}
	}
	return "$USER"
}
