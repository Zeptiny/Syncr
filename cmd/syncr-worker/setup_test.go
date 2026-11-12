package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/setup"
)

const (
	cmdWorkerID = "0b6f8f0e-52a5-4a3f-9a5d-2f2a7c9f1e11"
	cmdSecret   = "wsec_Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWo"
)

type cmdEnroller struct {
	mu   sync.Mutex
	reqs []setup.EnrollRequest
	err  error
}

func (c *cmdEnroller) Enroll(_ context.Context, req setup.EnrollRequest) (setup.Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return setup.Credentials{}, c.err
	}
	return setup.Credentials{WorkerID: cmdWorkerID, Secret: cmdSecret}, nil
}

type cmdServices struct {
	mu    sync.Mutex
	state service.State
	exe   string // the installed service's program
	calls []string
	// workDir is the working directory of the last install.
	workDir string
}

func (s *cmdServices) rec(c string) { s.mu.Lock(); s.calls = append(s.calls, c); s.mu.Unlock() }

func (s *cmdServices) Preflight(service.Spec) error { return nil }
func (s *cmdServices) Install(_ context.Context, sp service.Spec) error {
	s.rec("install " + strings.Join(sp.Args, " "))
	s.mu.Lock()
	s.state = service.StateStopped
	s.exe = sp.Executable
	s.workDir = sp.WorkDir
	s.mu.Unlock()
	return nil
}
func (s *cmdServices) Uninstall(context.Context, service.Spec) error {
	s.rec("uninstall")
	s.mu.Lock()
	s.state = service.StateNotInstalled
	s.mu.Unlock()
	return nil
}
func (s *cmdServices) Start(context.Context, service.Spec) error {
	s.rec("start")
	s.mu.Lock()
	s.state = service.StateRunning
	s.mu.Unlock()
	return nil
}
func (s *cmdServices) Stop(context.Context, service.Spec) error {
	s.rec("stop")
	s.mu.Lock()
	s.state = service.StateStopped
	s.mu.Unlock()
	return nil
}
func (s *cmdServices) Restart(context.Context, service.Spec) error { s.rec("restart"); return nil }
func (s *cmdServices) Status(context.Context, service.Spec) (service.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *cmdServices) InstalledExecutable(context.Context, service.Spec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == service.StateNotInstalled {
		return "", service.ErrNotInstalled
	}
	return s.exe, nil
}
func (s *cmdServices) Info(service.Spec) service.Info {
	return service.Info{Platform: "fake", Location: "fake://unit", LogHint: "fake-logs"}
}

type cmdTools struct{}

func (cmdTools) Provision(_ context.Context, toolsDir string, progress func(string)) ([]setup.ToolStatus, error) {
	progress("fake tools")
	return []setup.ToolStatus{
		{Name: "rclone", Version: "1", Source: "managed", Path: filepath.Join(toolsDir, "rclone")},
		{Name: "restic", Version: "2", Source: "managed", Path: filepath.Join(toolsDir, "restic")},
	}, nil
}

type setupFixture struct {
	dir      string
	services *cmdServices
	enroller *cmdEnroller
}

func newSetupFixture(t *testing.T, vars map[string]string) (*setupFixture, *env, func() string, func() string) {
	t.Helper()
	f := &setupFixture{dir: t.TempDir(), services: &cmdServices{}, enroller: &cmdEnroller{}}
	e, stdout, stderr := testEnv(vars)
	e.setup = setupOverrides{
		services:    f.services,
		enroller:    func(string, string) (setup.Enroller, error) { return f.enroller, nil },
		provisioner: cmdTools{},
		probe:       func(context.Context, string) (string, error) { return "", errors.New("not listening") },
		timing:      setup.Timing{Timeout: 100 * time.Millisecond, Interval: time.Millisecond, Stable: 2},
		executable:  func() (string, error) { return "/opt/syncr/syncr-worker", nil },
		hostname:    func() (string, error) { return "cmd-host", nil },
	}
	return f, e, stdout.String, stderr.String
}

func (f *setupFixture) args(extra ...string) []string {
	return append([]string{"setup", "--data-dir", filepath.Join(f.dir, "data"), "--config", filepath.Join(f.dir, "worker.yaml")}, extra...)
}

func TestSetupCommandActive(t *testing.T) {
	t.Parallel()
	f, e, stdout, stderr := newSetupFixture(t, map[string]string{tokenEnv: "wtok_from-environment"})
	root := filepath.Join(f.dir, "files")
	code := run(t.Context(), f.args("--controller", "https://syncr.example.com", "--local-root", root, "--name", "box"), e)
	if code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout(), stderr())
	}
	if len(f.enroller.reqs) != 1 {
		t.Fatalf("enrollments = %d", len(f.enroller.reqs))
	}
	req := f.enroller.reqs[0]
	if req.Token.Reveal() != "wtok_from-environment" || req.Name != "box" || req.Mode != workerconfig.LinkActive {
		t.Errorf("request = %+v: the token must come from $%s when --token is absent", req, tokenEnv)
	}
	cfg := workerconfig.Default()
	if err := platformconfig.Load(filepath.Join(f.dir, "worker.yaml"), nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Link.WorkerID != cmdWorkerID || cfg.Link.WorkerSecret.Reveal() != cmdSecret || cfg.LocalRoots[0] != root {
		t.Errorf("config = %+v", cfg)
	}
	if got := strings.Join(f.services.calls, "|"); !strings.Contains(got, "install run --config "+filepath.Join(f.dir, "worker.yaml")) || !strings.HasSuffix(got, "start") {
		t.Errorf("service calls = %q", got)
	}
	if !strings.Contains(stdout(), "Setup complete") {
		t.Errorf("stdout = %s", stdout())
	}
	for _, leak := range []string{"wtok_from-environment", cmdSecret} {
		if strings.Contains(stdout()+stderr(), leak) {
			t.Errorf("output leaks %q", leak)
		}
	}

	// A second run is refused with an actionable message and exit code 1.
	e2, _, stderr2 := testEnv(map[string]string{tokenEnv: "x"})
	e2.setup = e.setup
	if code := run(t.Context(), f.args("--controller", "https://syncr.example.com"), e2); code != exitError {
		t.Fatalf("second run exit %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr2.String(), "already set up") || !strings.Contains(stderr2.String(), "--force") {
		t.Errorf("stderr = %s", stderr2)
	}
}

// TestSetupCommandWritesTheToolsDir: tools installed under $SYNCR_TOOLS_DIR
// during setup must be found by the service, which runs without it.
func TestSetupCommandWritesTheToolsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	toolsDir := filepath.Join(dir, "shared-tools")
	f, e, stdout, stderr := newSetupFixture(t, map[string]string{tokenEnv: "wtok_x", toolsDirEnv: toolsDir})
	if code := run(t.Context(), f.args("--controller", "https://syncr.example.com"), e); code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout(), stderr())
	}
	cfg := workerconfig.Default()
	if err := platformconfig.Load(filepath.Join(f.dir, "worker.yaml"), nil, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Tools.Dir != toolsDir {
		t.Errorf("tools.dir = %q, want %q", cfg.Tools.Dir, toolsDir)
	}
}

func TestSetupCommandPassiveAndDryRun(t *testing.T) {
	t.Parallel()
	f, e, stdout, stderr := newSetupFixture(t, nil)
	args := f.args("--passive", "--listen", ":7443", "--advertise-url", "https://nas.example.com:7443", "--tls-name", "nas.lan", "--dry-run")
	if code := run(t.Context(), args, e); code != exitOK {
		t.Fatalf("dry run exit %d: %s", code, stderr())
	}
	if !strings.Contains(stdout(), "Dry run") {
		t.Errorf("stdout = %s", stdout())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "worker.yaml")); err == nil {
		t.Error("a dry run wrote the config")
	}
	if len(f.services.calls) != 0 || len(f.enroller.reqs) != 0 {
		t.Errorf("dry run touched collaborators: %v %v", f.services.calls, f.enroller.reqs)
	}

	f2, e2, stdout2, stderr2 := newSetupFixture(t, nil)
	args = f2.args("--passive", "--listen", ":7443", "--advertise-url", "https://nas.example.com:7443")
	if code := run(t.Context(), args, e2); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr2())
	}
	mustHave(t, stdout2(), "Pairing secret:", "Fingerprint:", "URL:             https://nas.example.com:7443")
	if len(f2.enroller.reqs) != 0 {
		t.Error("manual registration contacted a controller")
	}
}

func mustHave(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("output lacks %q:\n%s", w, text)
		}
	}
}

func TestSetupCommandErrorsAreActionable(t *testing.T) {
	t.Parallel()
	f, e, _, stderr := newSetupFixture(t, nil)
	f.enroller.err = &setup.EnrollError{Kind: setup.EnrollTLS, Message: "cannot verify the TLS certificate; pass --ca-file"}
	code := run(t.Context(), f.args("--controller", "https://syncr.example.com", "--token", "t"), e)
	if code != exitError {
		t.Fatalf("exit %d", code)
	}
	mustHave(t, stderr(), "syncr-worker setup:", "--ca-file")

	_, e, _, stderr = newSetupFixture(t, nil)
	if code := run(t.Context(), []string{"setup", "--controller", "https://x.example.com"}, e); code != exitError {
		t.Fatalf("exit %d", code)
	}
	mustHave(t, stderr(), "--token is required", tokenEnv)

	_, e, _, stderr = newSetupFixture(t, nil)
	if code := run(t.Context(), []string{"setup", "--bogus"}, e); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	mustHave(t, stderr(), "flag provided but not defined")
}

func TestServiceCommands(t *testing.T) {
	t.Parallel()
	f, e, stdout, stderr := newSetupFixture(t, nil)
	cfg := filepath.Join(f.dir, "worker.yaml")
	base := []string{"--config", cfg, "--data-dir", filepath.Join(f.dir, "data")}

	// install needs the config, which setup writes.
	if code := run(t.Context(), append([]string{"service", "install"}, base...), e); code != exitError || !strings.Contains(stderr(), "run 'syncr-worker setup' first") {
		t.Fatalf("install without config: exit %d, stderr %s", code, stderr())
	}
	if err := os.WriteFile(cfg, []byte("log:\n  level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(t.Context(), append([]string{"service", "install", "--start"}, base...), e); code != exitOK {
		t.Fatalf("install: exit %d, stderr %s", code, stderr())
	}
	mustHave(t, stdout(), "installed syncr-worker", "started syncr-worker")

	for _, action := range []string{"status", "restart", "stop"} {
		e, out, errOut := func() (*env, func() string, func() string) {
			e2, o, er := testEnv(nil)
			e2.setup = e.setup
			return e2, o.String, er.String
		}()
		if code := run(t.Context(), append([]string{"service", action}, base...), e); code != exitOK {
			t.Fatalf("%s: exit %d, stderr %s", action, code, errOut())
		}
		if action == "status" {
			mustHave(t, out(), "syncr-worker: running")
		}
	}

	// Status of a stopped service fails, for scripts.
	e3, out3, err3 := testEnv(nil)
	e3.setup = e.setup
	if code := run(t.Context(), append([]string{"service", "status"}, base...), e3); code != exitError {
		t.Errorf("status of a stopped service: exit %d, want %d", code, exitError)
	}
	mustHave(t, out3.String()+err3.String(), "stopped", "fake-logs")

	e4, _, err4 := testEnv(nil)
	e4.setup = e.setup
	if code := run(t.Context(), append([]string{"service", "uninstall"}, base...), e4); code != exitOK {
		t.Fatalf("uninstall: exit %d %s", code, err4)
	}
	if got := f.services.state; got != service.StateNotInstalled {
		t.Errorf("state = %v", got)
	}

	e5, _, err5 := testEnv(nil)
	if code := run(t.Context(), []string{"service", "frobnicate"}, e5); code != exitUsage {
		t.Errorf("unknown action: exit %d", code)
	}
	if code := run(t.Context(), []string{"service", "stop", "--start"}, e5); code != exitUsage {
		t.Errorf("--start with stop: exit %d (%s)", code, err5)
	}
}

// TestServiceInstallUsesTheConfiguredDataDir: after `setup --no-service`
// with a --data-dir of its own, `service install` as setup's hint says
// (without --data-dir) runs the service in the configured data directory,
// not the platform default, and the hint names the config.
func TestServiceInstallUsesTheConfiguredDataDir(t *testing.T) {
	t.Parallel()
	f, e, stdout, stderr := newSetupFixture(t, map[string]string{tokenEnv: "wtok_x"})
	cfg := filepath.Join(f.dir, "worker.yaml")
	if code := run(t.Context(), f.args("--controller", "https://syncr.example.com", "--no-service"), e); code != exitOK {
		t.Fatalf("setup: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout(), stderr())
	}
	mustHave(t, stdout(), "service install --start --config "+cfg)

	// This terminal's $SYNCR_DATA_DIR does not reach the service, so the
	// config wins over it.
	e2, _, stderr2 := testEnv(map[string]string{dataDirEnv: filepath.Join(f.dir, "elsewhere")})
	e2.setup = e.setup
	if code := run(t.Context(), []string{"service", "install", "--config", cfg}, e2); code != exitOK {
		t.Fatalf("service install: exit %d, %s", code, stderr2.String())
	}
	f.services.mu.Lock()
	defer f.services.mu.Unlock()
	if want := filepath.Join(f.dir, "data"); f.services.workDir != want {
		t.Errorf("service working directory = %q, want the configured %q", f.services.workDir, want)
	}
}

// TestRelativePathsReachTheServiceAbsolute runs `setup` and `service install`
// with relative --config and --data-dir: the service starts elsewhere, so it
// must be given absolute paths. Not parallel: it changes the working
// directory.
func TestRelativePathsReachTheServiceAbsolute(t *testing.T) {
	f, e, stdout, stderr := newSetupFixture(t, map[string]string{tokenEnv: "wtok_x"})
	t.Chdir(f.dir)
	cfg := filepath.Join(f.dir, "worker.yaml")
	if code := run(t.Context(), []string{"setup", "--controller", "https://syncr.example.com", "--config", "worker.yaml", "--data-dir", "data"}, e); code != exitOK {
		t.Fatalf("setup: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout(), stderr())
	}
	loaded := workerconfig.Default()
	if err := platformconfig.Load(cfg, nil, &loaded); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(loaded.DataDir) || filepath.Base(loaded.DataDir) != "data" {
		t.Errorf("data_dir = %q, want an absolute path", loaded.DataDir)
	}

	e2, _, stderr2 := testEnv(nil)
	e2.setup = e.setup
	if code := run(t.Context(), []string{"service", "install", "--config", "worker.yaml", "--data-dir", "data"}, e2); code != exitOK {
		t.Fatalf("service install: exit %d, %s", code, stderr2)
	}

	f.services.mu.Lock()
	calls := append([]string(nil), f.services.calls...)
	f.services.mu.Unlock()
	var installs []string
	for _, c := range calls {
		if strings.HasPrefix(c, "install ") {
			installs = append(installs, c)
		}
	}
	if len(installs) != 2 {
		t.Fatalf("install calls = %q, want one from setup and one from service install", installs)
	}
	for _, c := range installs {
		got := strings.TrimPrefix(c, "install run --config ")
		if !filepath.IsAbs(got) || filepath.Base(got) != "worker.yaml" {
			t.Errorf("service installed with --config %q, want an absolute path", got)
		}
	}
}

// fakeHost stands in for a service manager that started the process.
type fakeHost struct {
	mu     sync.Mutex
	ran    bool
	logOut *strings.Builder
}

func (h *fakeHost) Managed() bool { return true }

func (h *fakeHost) Run(ctx context.Context, svcName string, _ io.Writer, fn func(context.Context, io.Writer) error) error {
	h.mu.Lock()
	h.ran = svcName == service.Name
	h.mu.Unlock()
	return fn(ctx, writerFunc(func(p []byte) (int, error) { return h.logOut.Write(p) }))
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestRunUnderServiceManagerUsesTheHost(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	f := newFixture(t)
	e, stderr := f.runEnv(nil)
	host := &fakeHost{logOut: &strings.Builder{}}
	e.host = host
	ready := notifyReady(e)

	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, e) }()
	awaitReady(t, ready, done, stderr)
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	host.mu.Lock()
	ran := host.ran
	host.mu.Unlock()
	if !ran {
		t.Error("run did not go through the service host under the name syncr-worker")
	}
	if !strings.Contains(host.logOut.String(), `"msg":"worker ready"`) {
		t.Errorf("worker logs did not go to the host's log writer: %q", host.logOut.String())
	}
	if strings.Contains(stderr.String(), "worker ready") {
		t.Error("logs went to stderr under a service manager")
	}
}

func TestRunReturnsStartupErrorThroughTheHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, stderr := f.runEnv(map[string]string{"SYNCR_RCLONE_PATH": "", "SYNCR_RESTIC_PATH": ""})
	e.host = &fakeHost{logOut: &strings.Builder{}}
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "required tools are not available") {
		t.Errorf("stderr = %s", stderr)
	}
}

func TestRunStartsAndStopsTheLink(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	f := newFixture(t)
	e, stderr := f.runEnv(map[string]string{
		"SYNCR_LINK_MODE":      "active",
		"SYNCR_CONTROLLER_URL": "https://syncr.example.com",
		"SYNCR_WORKER_ID":      cmdWorkerID,
		"SYNCR_WORKER_SECRET":  cmdSecret,
	})
	var (
		mu      sync.Mutex
		started workerconfig.LinkMode
		stopped bool
		gotW    *worker
	)
	e.link = func(_ context.Context, cfg workerconfig.Config, w *worker, _ *slog.Logger) (func(), error) {
		mu.Lock()
		defer mu.Unlock()
		started, gotW = cfg.Link.Mode, w
		return func() { mu.Lock(); stopped = true; mu.Unlock() }, nil
	}
	ready := notifyReady(e)
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, e) }()
	awaitReady(t, ready, done, stderr)
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if started != workerconfig.LinkActive || gotW == nil || gotW.registry == nil || !stopped {
		t.Errorf("link hook: mode %q, worker %v, stopped %v", started, gotW, stopped)
	}
	if strings.Contains(stderr.String(), cmdSecret) {
		t.Error("worker secret in the log")
	}
}

func TestRunLinkErrorFailsStartup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, stderr := f.runEnv(nil)
	e.link = func(context.Context, workerconfig.Config, *worker, *slog.Logger) (func(), error) {
		return nil, errors.New("cannot listen on :7443")
	}
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot listen on :7443") {
		t.Errorf("stderr = %s", stderr)
	}
}

func TestRunLogFileAndHideConsole(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, stderr := f.runEnv(nil)
	if code := run(t.Context(), []string{"run", "--hide-console"}, e); code != exitError || !strings.Contains(stderr.String(), "--hide-console needs --log-file") {
		t.Errorf("exit/stderr = %s", stderr)
	}

	ctx, cancel := context.WithCancel(t.Context())
	logPath := filepath.Join(t.TempDir(), "logs", "worker.log")
	e, stderr = f.runEnv(nil)
	ready := notifyReady(e)
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run", "--log-file", logPath}, e) }()
	awaitReady(t, ready, done, stderr)
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "worker ready") || strings.Contains(stderr.String(), "worker ready") {
		t.Errorf("log file = %q, stderr = %q", body, stderr)
	}
}
