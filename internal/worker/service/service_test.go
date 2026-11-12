package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kservice "github.com/kardianos/service"
)

func testSpec(t *testing.T, scope Scope) Spec {
	t.Helper()
	// Fixed Unix paths: the manager under test validates them as a Linux one
	// does, on every host.
	return NewSpec("linux", scope, "/opt/syncr/syncr-worker", "/etc/syncr/worker.yaml", "/var/lib/syncr", "")
}

func TestNewSpec(t *testing.T) {
	t.Parallel()
	exe, cfg, data := filepath.FromSlash("/opt/syncr-worker"), filepath.FromSlash("/etc/syncr/worker.yaml"), filepath.FromSlash("/var/lib/syncr")
	logFile := filepath.Join(data, "logs", "worker.log")

	linux := NewSpec("linux", ScopeSystem, exe, cfg, data, logFile)
	if want := []string{"run", "--config", cfg}; !slices.Equal(linux.Args, want) {
		t.Errorf("linux args = %q, want %q", linux.Args, want)
	}
	if linux.LogFile != "" || linux.Name != Name || linux.WorkDir != data {
		t.Errorf("linux spec = %+v", linux)
	}

	winSvc := NewSpec("windows", ScopeSystem, exe, cfg, data, logFile)
	if len(winSvc.Args) != 3 {
		t.Errorf("windows service args = %q, want only run --config (the Event Log is used)", winSvc.Args)
	}

	winUser := NewSpec("windows", ScopeUser, exe, cfg, data, logFile)
	if want := []string{"run", "--config", cfg, "--log-file", logFile, "--hide-console"}; !slices.Equal(winUser.Args, want) {
		t.Errorf("windows user args = %q, want %q", winUser.Args, want)
	}
	if winUser.LogFile != logFile {
		t.Errorf("windows user LogFile = %q", winUser.LogFile)
	}
}

func TestSpecValidate(t *testing.T) {
	t.Parallel()
	good := testSpec(t, ScopeSystem)
	if err := good.validate("linux"); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Executable = "syncr-worker"
	if err := bad.validate("linux"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative executable: err = %v", err)
	}
	bad = good
	bad.Name = "has space"
	if err := bad.validate("linux"); err == nil {
		t.Error("name with a space accepted")
	}
	win := NewSpec("windows", ScopeSystem, `C:\Program Files\Syncr\syncr-worker.exe`, `C:\ProgramData\Syncr\worker.yaml`, `C:\ProgramData\Syncr`, "")
	if err := win.validate("windows"); err != nil {
		t.Errorf("windows path rejected: %v", err)
	}
	win.Executable = "/opt/syncr-worker"
	if err := win.validate("windows"); err == nil {
		t.Error("unix path accepted on windows")
	}

	// The service starts in another directory than the installer ran in,
	// so every path it is given must be absolute.
	for _, tt := range []struct {
		name string
		spec Spec
		want string
	}{
		{"relative config", NewSpec("linux", ScopeSystem, "/opt/syncr-worker", "worker.yaml", "/var/lib/syncr", ""), `--config "worker.yaml"`},
		{"relative working directory", NewSpec("linux", ScopeSystem, "/opt/syncr-worker", "/etc/syncr/worker.yaml", "data", ""), `working directory "data"`},
		{"relative log file", NewSpec("windows", ScopeUser, `C:\w.exe`, `C:\c.yaml`, `C:\d`, `logs\worker.log`), `--log-file "logs\\worker.log"`},
		{"unix config on windows", NewSpec("windows", ScopeSystem, `C:\w.exe`, "/etc/syncr/worker.yaml", `C:\d`, ""), "--config"},
	} {
		goos := "linux"
		if strings.HasPrefix(tt.spec.Executable, `C:`) {
			goos = "windows"
		}
		if err := tt.spec.validate(goos); err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "not an absolute path") {
			t.Errorf("%s: err = %v, want it to name %s", tt.name, err, tt.want)
		}
	}
	if err := NewSpec("windows", ScopeUser, `C:\w.exe`, `C:\c.yaml`, `C:\d`, `C:\d\logs\worker.log`).validate("windows"); err != nil {
		t.Errorf("absolute windows user spec rejected: %v", err)
	}
}

// fakeService records the calls the manager makes on a kardianos service.
type fakeService struct {
	mu       sync.Mutex
	cfg      *kservice.Config
	calls    []string
	status   kservice.Status
	statusEr error
	failOn   map[string]error
}

func (f *fakeService) call(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.failOn[name]
}

func (f *fakeService) Run() error       { return f.call("run") }
func (f *fakeService) Start() error     { return f.call("start") }
func (f *fakeService) Stop() error      { return f.call("stop") }
func (f *fakeService) Restart() error   { return f.call("restart") }
func (f *fakeService) Install() error   { return f.call("install") }
func (f *fakeService) Uninstall() error { return f.call("uninstall") }
func (f *fakeService) Logger(chan<- error) (kservice.Logger, error) {
	return nil, errors.New("no logger")
}
func (f *fakeService) SystemLogger(chan<- error) (kservice.Logger, error) {
	return nil, errors.New("no logger")
}
func (f *fakeService) String() string   { return f.cfg.Name }
func (f *fakeService) Platform() string { return "fake" }
func (f *fakeService) Status() (kservice.Status, error) {
	return f.status, f.statusEr
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	reply func(name string, args []string) (string, int, error)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	r.mu.Unlock()
	if r.reply == nil {
		return "", 0, nil
	}
	return r.reply(name, args)
}

type harness struct {
	m      *manager
	svc    *fakeService
	runner *fakeRunner
	// imagePath is the Windows Service's command line; "" means none.
	imagePath string
}

func newHarness(goos, platform string, elevated bool) *harness {
	h := &harness{svc: &fakeService{status: kservice.StatusStopped, statusEr: kservice.ErrNotInstalled}, runner: &fakeRunner{}}
	h.m = newManager(deps{
		goos:     goos,
		elevated: func() bool { return elevated },
		lookupEnv: func(k string) (string, bool) {
			return map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "HOME": "/home/u", "USER": "u"}[k], k != "NOPE"
		},
		platform: func() string { return platform },
		newService: func(_ kservice.Interface, c *kservice.Config) (kservice.Service, error) {
			h.svc.cfg = c
			return h.svc, nil
		},
		runner:   h.runner,
		taskUser: func() (string, error) { return `PC\alice`, nil },
		tempDir:  os.TempDir,
		serviceCommandLine: func(name string) (string, error) {
			if h.imagePath == "" {
				return "", fmt.Errorf("%s: %w", name, ErrNotInstalled)
			}
			return h.imagePath, nil
		},
		sleep: func(context.Context, time.Duration) error { return nil },
	})
	return h
}

func TestInstallSystemdSystemConfig(t *testing.T) {
	t.Parallel()
	h := newHarness("linux", "linux-systemd", true)
	h.runner.reply = func(string, []string) (string, int, error) {
		return "LoadState=not-found\nActiveState=inactive\nSubState=dead", 0, nil
	}
	spec := testSpec(t, ScopeSystem)
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.svc.calls, []string{"install"}) {
		t.Errorf("calls = %v", h.svc.calls)
	}
	cfg := h.svc.cfg
	if cfg.Name != Name || cfg.Executable != spec.Executable || !slices.Equal(cfg.Arguments, spec.Args) || cfg.WorkingDirectory != spec.WorkDir {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Option["UserService"] != false {
		t.Errorf("UserService = %v, want false", cfg.Option["UserService"])
	}
	script, _ := cfg.Option["SystemdScript"].(string)
	for _, want := range []string{"Restart=on-failure", "RestartSec=5", "WantedBy=multi-user.target", "NoNewPrivileges=yes"} {
		if !strings.Contains(script, want) {
			t.Errorf("system unit template lacks %q", want)
		}
	}
}

// TestInstallSystemdEscapes: '%' (a specifier) and, in the arguments, '$' (a
// variable) reach the unit doubled, so systemd reads the paths as given;
// other service managers get them unchanged.
func TestInstallSystemdEscapes(t *testing.T) {
	t.Parallel()
	spec := NewSpec("linux", ScopeSystem, "/opt/sync%r/syncr-worker", "/srv/backup%20data/$HOME/worker.yaml", "/var/lib/syncr%h", "")
	h := newHarness("linux", "linux-systemd", true)
	h.runner.reply = func(string, []string) (string, int, error) { return "LoadState=not-found", 0, nil }
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	cfg := h.svc.cfg
	if cfg.Executable != "/opt/sync%%r/syncr-worker" || cfg.WorkingDirectory != "/var/lib/syncr%%h" ||
		!slices.Equal(cfg.Arguments, []string{"run", "--config", "/srv/backup%%20data/$$HOME/worker.yaml"}) {
		t.Errorf("config = %q %q %q", cfg.Executable, cfg.WorkingDirectory, cfg.Arguments)
	}
	if spec.Args[2] != "/srv/backup%20data/$HOME/worker.yaml" {
		t.Errorf("the caller's spec was changed: %q", spec.Args)
	}

	h = newHarness("linux", "linux-openrc", true)
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if cfg := h.svc.cfg; cfg.Executable != spec.Executable || !slices.Equal(cfg.Arguments, spec.Args) {
		t.Errorf("openrc config = %q %q", cfg.Executable, cfg.Arguments)
	}
}

func TestInstallSystemdUserConfig(t *testing.T) {
	t.Parallel()
	h := newHarness("linux", "linux-systemd", false)
	h.runner.reply = func(string, []string) (string, int, error) { return "LoadState=not-found", 0, nil }
	spec := testSpec(t, ScopeUser)
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatalf("user install without root: %v", err)
	}
	if h.svc.cfg.Option["UserService"] != true {
		t.Errorf("UserService = %v, want true", h.svc.cfg.Option["UserService"])
	}
	script, _ := h.svc.cfg.Option["SystemdScript"].(string)
	if !strings.Contains(script, "WantedBy=default.target") || strings.Contains(script, "multi-user.target") {
		t.Error("user unit must be wanted by default.target, which the user manager has")
	}
	if h.runner.calls[0] != "systemctl --user show -p LoadState -p ActiveState -p SubState syncr-worker.service" {
		t.Errorf("status query = %q", h.runner.calls[0])
	}
	info := h.m.Info(spec)
	if !strings.Contains(strings.Join(info.Notes, " "), "loginctl enable-linger") || !strings.HasPrefix(info.LogHint, "journalctl --user") {
		t.Errorf("info = %+v", info)
	}
}

func TestInstallWindowsServiceConfig(t *testing.T) {
	t.Parallel()
	h := newHarness("windows", "windows-service", true)
	spec := NewSpec("windows", ScopeSystem, `C:\Syncr\syncr-worker.exe`, `C:\ProgramData\Syncr\worker.yaml`, `C:\ProgramData\Syncr`, "")
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	opt := h.svc.cfg.Option
	if opt["StartType"] != "automatic" || opt["OnFailure"] != "restart" || opt["DelayedAutoStart"] != true {
		t.Errorf("windows options = %v", opt)
	}
	if _, ok := opt["SystemdScript"]; ok {
		t.Error("systemd template set for a Windows service")
	}
	if h.svc.cfg.WorkingDirectory != "" {
		t.Errorf("WorkingDirectory = %q; the SCM has none", h.svc.cfg.WorkingDirectory)
	}
}

func TestPreflight(t *testing.T) {
	t.Parallel()
	t.Run("system needs root", func(t *testing.T) {
		t.Parallel()
		h := newHarness("linux", "linux-systemd", false)
		err := h.m.Install(t.Context(), testSpec(t, ScopeSystem))
		var perm *PermissionError
		if !errors.As(err, &perm) || !strings.Contains(err.Error(), "--user") || !strings.Contains(err.Error(), "root") {
			t.Fatalf("err = %v, want a PermissionError suggesting root or --user", err)
		}
		if len(h.svc.calls) != 0 {
			t.Errorf("service touched despite missing rights: %v", h.svc.calls)
		}
	})
	t.Run("windows system needs administrator", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", false)
		err := h.m.Preflight(NewSpec("windows", ScopeSystem, `C:\w.exe`, `C:\c.yaml`, `C:\d`, ""))
		var perm *PermissionError
		if !errors.As(err, &perm) || !strings.Contains(err.Error(), "elevated") || !strings.Contains(err.Error(), "--user") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("windows user needs nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", false)
		if err := h.m.Preflight(NewSpec("windows", ScopeUser, `C:\w.exe`, `C:\c.yaml`, `C:\d`, `C:\l.log`)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("user without a session bus", func(t *testing.T) {
		t.Parallel()
		h := newHarness("linux", "linux-systemd", false)
		h.m.lookupEnv = func(string) (string, bool) { return "", false }
		err := h.m.Preflight(testSpec(t, ScopeUser))
		if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("user needs systemd", func(t *testing.T) {
		t.Parallel()
		h := newHarness("linux", "unix-systemv", false)
		err := h.m.Preflight(testSpec(t, ScopeUser))
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
	})
}

func TestInstallRefusesExisting(t *testing.T) {
	t.Parallel()
	h := newHarness("windows", "windows-service", true)
	h.svc.status, h.svc.statusEr = kservice.StatusRunning, nil
	err := h.m.Install(t.Context(), NewSpec("windows", ScopeSystem, `C:\w.exe`, `C:\c.yaml`, `C:\d`, ""))
	if !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("err = %v, want ErrAlreadyInstalled", err)
	}
	if len(h.svc.calls) != 0 {
		t.Errorf("calls = %v", h.svc.calls)
	}
}

func TestControlAndUninstall(t *testing.T) {
	t.Parallel()
	spec := NewSpec("windows", ScopeSystem, `C:\w.exe`, `C:\c.yaml`, `C:\d`, "")

	t.Run("not installed", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", true)
		for name, fn := range map[string]func(context.Context, Spec) error{"start": h.m.Start, "stop": h.m.Stop, "restart": h.m.Restart, "uninstall": h.m.Uninstall} {
			if err := fn(t.Context(), spec); !errors.Is(err, ErrNotInstalled) {
				t.Errorf("%s: err = %v, want ErrNotInstalled", name, err)
			}
		}
	})
	t.Run("installed", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", true)
		h.svc.status, h.svc.statusEr = kservice.StatusRunning, nil
		for _, fn := range []func(context.Context, Spec) error{h.m.Start, h.m.Stop, h.m.Restart, h.m.Uninstall} {
			if err := fn(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
		}
		// Uninstall stops a running service first.
		if want := []string{"start", "stop", "restart", "stop", "uninstall"}; !slices.Equal(h.svc.calls, want) {
			t.Errorf("calls = %v, want %v", h.svc.calls, want)
		}
	})
	t.Run("restarting a stopped service starts it", func(t *testing.T) {
		t.Parallel()
		// The SCM refuses to stop a service that is not running, so a
		// restart (stop, then start) of a stopped one would fail.
		h := newHarness("windows", "windows-service", true)
		h.svc.status, h.svc.statusEr = kservice.StatusStopped, nil
		h.svc.failOn = map[string]error{"restart": errors.New("the service has not been started")}
		if err := h.m.Restart(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(h.svc.calls, []string{"start"}) {
			t.Errorf("calls = %v, want start", h.svc.calls)
		}
		// Stopping it again is nothing to do, not an error.
		h.svc.failOn = map[string]error{"stop": errors.New("the service has not been started")}
		if err := h.m.Stop(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(h.svc.calls, []string{"start"}) {
			t.Errorf("calls = %v, want no stop", h.svc.calls)
		}
	})
	t.Run("uninstall removes a service whose stop fails", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", true)
		h.svc.status, h.svc.statusEr = kservice.StatusRunning, nil
		h.svc.failOn = map[string]error{"stop": errors.New("stuck")}
		if err := h.m.Uninstall(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(h.svc.calls, "uninstall") {
			t.Errorf("calls = %v, want uninstall despite the failing stop", h.svc.calls)
		}
	})
	t.Run("access denied becomes a permission error", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", true)
		h.svc.status, h.svc.statusEr = kservice.StatusStopped, nil
		h.svc.failOn = map[string]error{"start": errors.New("access is denied")}
		var perm *PermissionError
		if err := h.m.Start(t.Context(), spec); !errors.As(err, &perm) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStatusMapping(t *testing.T) {
	t.Parallel()
	spec := NewSpec("windows", ScopeSystem, `C:\w.exe`, `C:\c.yaml`, `C:\d`, "")
	tests := []struct {
		status kservice.Status
		err    error
		want   State
	}{
		{kservice.StatusRunning, nil, StateRunning},
		{kservice.StatusStopped, nil, StateStopped},
		{kservice.StatusUnknown, kservice.ErrNotInstalled, StateNotInstalled},
	}
	for _, tt := range tests {
		h := newHarness("windows", "windows-service", false)
		h.svc.status, h.svc.statusEr = tt.status, tt.err
		got, err := h.m.Status(t.Context(), spec)
		if err != nil || got != tt.want {
			t.Errorf("Status(%v, %v) = %v, %v; want %v", tt.status, tt.err, got, err, tt.want)
		}
	}
	h := newHarness("windows", "windows-service", false)
	h.svc.status, h.svc.statusEr = kservice.StatusUnknown, errors.New("boom")
	if _, err := h.m.Status(t.Context(), spec); err == nil {
		t.Error("a query error was swallowed")
	}
}

func TestSystemdState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		props map[string]string
		want  State
	}{
		{map[string]string{"LoadState": "not-found", "ActiveState": "inactive"}, StateNotInstalled},
		{map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "running"}, StateRunning},
		{map[string]string{"LoadState": "loaded", "ActiveState": "activating", "SubState": "start"}, StateRunning},
		{map[string]string{"LoadState": "loaded", "ActiveState": "activating", "SubState": "auto-restart"}, StateFailing},
		{map[string]string{"LoadState": "loaded", "ActiveState": "failed", "SubState": "failed"}, StateFailing},
		{map[string]string{"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead"}, StateStopped},
	}
	for _, tt := range tests {
		if got := parseSystemdState(tt.props); got != tt.want {
			t.Errorf("parseSystemdState(%v) = %v, want %v", tt.props, got, tt.want)
		}
	}

	r := &fakeRunner{reply: func(string, []string) (string, int, error) {
		return "Failed to connect to bus: No medium found", 1, nil
	}}
	_, err := systemdState(t.Context(), r, testSpec(t, ScopeUser))
	if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Errorf("err = %v, want the bus hint", err)
	}
	r = &fakeRunner{reply: func(string, []string) (string, int, error) { return "", -1, fmt.Errorf("systemctl: not found") }}
	if _, err := systemdState(t.Context(), r, testSpec(t, ScopeSystem)); err == nil {
		t.Error("missing systemctl not reported")
	}
}

func TestStateStrings(t *testing.T) {
	t.Parallel()
	for s, want := range map[State]string{StateNotInstalled: "not installed", StateStopped: "stopped", StateRunning: "running"} {
		if s.String() != want {
			t.Errorf("%d = %q, want %q", s, s.String(), want)
		}
	}
	if ScopeUser.String() != "user" || ScopeSystem.String() != "system" {
		t.Error("scope strings")
	}
}

// TestUnitTemplatesStopOnlyTheWorker guards the shutdown order of ADR 0005:
// systemd must signal only the worker, which stops its rclone and restic
// children itself, and must wait longer than the worker's stop budget before
// it kills what is left.
func TestUnitTemplatesStopOnlyTheWorker(t *testing.T) {
	t.Parallel()
	for scope, tmpl := range map[Scope]string{ScopeSystem: systemUnitTemplate, ScopeUser: userUnitTemplate} {
		var killMode, timeoutStop string
		for line := range strings.SplitSeq(tmpl, "\n") {
			if v, ok := strings.CutPrefix(line, "KillMode="); ok {
				killMode = v
			}
			if v, ok := strings.CutPrefix(line, "TimeoutStopSec="); ok {
				timeoutStop = v
			}
		}
		if killMode != "mixed" {
			t.Errorf("%s unit: KillMode=%q, want mixed (control-group signals the tools together with the worker)", scope, killMode)
		}
		d, err := time.ParseDuration(timeoutStop + "s")
		if err != nil {
			t.Fatalf("%s unit: TimeoutStopSec=%q: %v", scope, timeoutStop, err)
		}
		if d <= stopTimeout {
			t.Errorf("%s unit: TimeoutStopSec=%s does not exceed the worker's stop budget of %s", scope, d, stopTimeout)
		}
	}
}
