package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
	"github.com/Zeptiny/Syncr/internal/worker/upgrade"
)

// The versions of the upgrade tests: what testEnv's binary reports, and
// the release the test server offers.
const (
	installedVersion = "v2.0.0-test"
	offeredVersion   = "v2.1.0"
)

// release serves a release's checksums.txt and this platform's worker
// archive, whose binary is a text file naming its version (fakeBinary
// reads it).
type release struct {
	srv      *httptest.Server
	archive  string
	checksum string // checksums.txt
}

// newRelease serves version under /. checksums is checksums.txt; "" means
// the archive's correct line.
func newRelease(t *testing.T, version string, checksums func(name, sum string) string) *release {
	t.Helper()
	member, name := "syncr-worker", fmt.Sprintf("syncr-worker_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	body := []byte("binary " + version + "\n")
	if runtime.GOOS == "windows" {
		member, name = "syncr-worker.exe", fmt.Sprintf("syncr-worker_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	r := &release{archive: name, checksum: hex.EncodeToString(sum[:]) + "  " + name + "\n"}
	if checksums != nil {
		r.checksum = checksums(name, hex.EncodeToString(sum[:]))
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/checksums.txt":
			_, _ = w.Write([]byte(r.checksum))
		case "/" + name:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// fakeBinary runs the downloaded "binary": its version is in its text.
type fakeBinary struct {
	mu        sync.Mutex
	toolsDir  string
	toolPaths map[string]string
	toolsErr  error
}

func (b *fakeBinary) Version(_ context.Context, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(raw))
	if len(f) != 2 || f[0] != "binary" {
		return "", fmt.Errorf("%s is not a fake binary", path)
	}
	return f[1], nil
}

func (b *fakeBinary) FetchTools(_ context.Context, _, toolsDir string) ([]tools.ToolVersion, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toolsDir = toolsDir
	return []tools.ToolVersion{{Tool: "rclone", Version: "v1.99.0"}, {Tool: "restic", Version: "0.99.0"}}, nil
}

func (b *fakeBinary) CheckTools(_ context.Context, _, _ string, configured map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toolPaths = configured
	return b.toolsErr
}

// upgradeServices is a service manager whose service runs until it is
// restarted into state afterRestart.
type upgradeServices struct {
	mu           sync.Mutex
	state        service.State
	afterRestart service.State
	preflight    error
	specs        []service.Spec
	calls        []string
}

func (s *upgradeServices) rec(call string, spec service.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	s.specs = append(s.specs, spec)
}

func (s *upgradeServices) Preflight(spec service.Spec) error {
	s.rec("preflight", spec)
	return s.preflight
}
func (s *upgradeServices) Install(context.Context, service.Spec) error {
	return errors.New("upgrade must not install")
}
func (s *upgradeServices) Uninstall(context.Context, service.Spec) error {
	return errors.New("upgrade must not uninstall")
}
func (s *upgradeServices) Start(_ context.Context, spec service.Spec) error {
	s.rec("start", spec)
	s.mu.Lock()
	s.state = service.StateRunning
	s.mu.Unlock()
	return nil
}
func (s *upgradeServices) Stop(_ context.Context, spec service.Spec) error {
	s.rec("stop", spec)
	s.mu.Lock()
	s.state = service.StateStopped
	s.mu.Unlock()
	return nil
}
func (s *upgradeServices) Restart(_ context.Context, spec service.Spec) error {
	s.rec("restart", spec)
	s.mu.Lock()
	s.state = s.afterRestart
	s.mu.Unlock()
	return nil
}
func (s *upgradeServices) Status(context.Context, service.Spec) (service.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *upgradeServices) Info(service.Spec) service.Info {
	return service.Info{Platform: "fake", LogHint: "journalctl -u syncr-worker"}
}

func (s *upgradeServices) changes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.calls), func(c string) bool { return c == "preflight" })
}

// fakePruner records what it was told to keep.
type fakePruner struct {
	mu   sync.Mutex
	keep []tools.ToolVersion
}

func (p *fakePruner) Prune(_ context.Context, keep []tools.ToolVersion) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keep = keep
	return []string{"/tools/rclone/v1.75.2"}, nil
}

// upgradeFixture is an installed worker: its binary, config and service.
type upgradeFixture struct {
	dir, exe, config, dataDir string
	services                  *upgradeServices
	binary                    *fakeBinary
	pruner                    *fakePruner
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	dir := t.TempDir()
	f := &upgradeFixture{
		dir: dir, exe: filepath.Join(dir, "bin", "syncr-worker"), config: filepath.Join(dir, "worker.yaml"), dataDir: filepath.Join(dir, "data"),
		services: &upgradeServices{state: service.StateRunning, afterRestart: service.StateRunning},
		binary:   &fakeBinary{}, pruner: &fakePruner{},
	}
	if runtime.GOOS == "windows" {
		f.exe += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(f.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.exe, []byte("binary "+installedVersion+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rclone := filepath.Join(dir, "bin", "rclone")
	cfg := fmt.Sprintf("data_dir: %q\ntools:\n  rclone_path: %q\n", f.dataDir, rclone)
	if err := os.WriteFile(f.config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// run runs `upgrade` with args and returns the exit code and the output.
func (f *upgradeFixture) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	e, out, errOut := testEnv(nil)
	e.setup.services = f.services
	e.setup.executable = func() (string, error) { return f.exe, nil }
	e.upgrade = upgradeOverrides{
		binary: f.binary, pruner: f.pruner,
		sleep:        func(context.Context, time.Duration) error { return nil },
		healthWindow: 3 * time.Second,
	}
	code = run(t.Context(), append([]string{"upgrade"}, args...), e)
	return code, out.String(), errOut.String()
}

// installed returns the version of the installed binary.
func (f *upgradeFixture) installed(t *testing.T) string {
	t.Helper()
	v, err := f.binary.Version(t.Context(), f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpgradeCommand(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	r := newRelease(t, offeredVersion, nil)
	code, stdout, stderr := f.run(t, "--from", r.srv.URL, "--config", f.config)
	if code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got := f.installed(t); got != offeredVersion {
		t.Errorf("installed %s, want %s", got, offeredVersion)
	}
	for _, want := range []string{"Verified " + r.archive, "Upgraded from " + installedVersion + " to " + offeredVersion,
		"The worker binary " + f.exe + " is now " + offeredVersion} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if got := f.services.changes(); !slices.Equal(got, []string{"restart"}) {
		t.Errorf("service calls = %v, want one restart", got)
	}
	// The service, the tools directory and the tool paths are the
	// configured worker's.
	spec := f.services.specs[0]
	if spec.Name != service.Name || spec.Scope != service.ScopeSystem || spec.Executable != f.exe || !slices.Contains(spec.Args, f.config) {
		t.Errorf("service spec = %+v", spec)
	}
	if f.binary.toolsDir != filepath.Join(f.dataDir, "tools") || f.binary.toolPaths["rclone"] != filepath.Join(f.dir, "bin", "rclone") {
		t.Errorf("tools %s, paths %v", f.binary.toolsDir, f.binary.toolPaths)
	}
	if len(f.pruner.keep) != 2 {
		t.Errorf("pruned keeping %v", f.pruner.keep)
	}
	if _, err := os.Stat(f.exe + upgrade.BackupSuffix); !os.IsNotExist(err) {
		t.Errorf("the old binary was left behind: %v", err)
	}
}

func TestUpgradeCommandOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup prepares the fixture; args follow --from and --config.
		setup     func(f *upgradeFixture)
		checksums func(name, sum string) string
		// offer is the version the release holds (default offeredVersion).
		offer    string
		args     []string
		wantCode int
		// wantVersion is the version installed afterwards.
		wantVersion string
		wantCalls   []string
		wantOut     []string
		wantErr     []string
	}{
		{name: "already installed", offer: installedVersion, args: []string{"--version", installedVersion}, wantCode: exitOK, wantVersion: installedVersion,
			wantOut: []string{installedVersion + " is already installed; nothing to do"}},
		{name: "dry run", args: []string{"--dry-run"}, wantCode: exitOK, wantVersion: installedVersion,
			wantOut: []string{"Dry run: the release checks out", "restart the syncr-worker service", "Nothing was changed."}},
		{name: "no restart", args: []string{"--no-restart"}, wantCode: exitOK, wantVersion: offeredVersion,
			wantOut: []string{"Restart the worker to run " + offeredVersion, "is now " + offeredVersion}},
		{name: "rolled back", setup: func(f *upgradeFixture) { f.services.afterRestart = service.StateFailing },
			wantCode: exitError, wantVersion: installedVersion, wantCalls: []string{"restart", "stop", "start"},
			wantErr: []string{"the upgrade was rolled back", offeredVersion + " did not stay running", installedVersion + " was put back and started",
				"journalctl -u syncr-worker"}},
		{name: "tampered archive", checksums: func(name, _ string) string { return strings.Repeat("ab", 32) + "  " + name + "\n" },
			wantCode: exitError, wantVersion: installedVersion, wantErr: []string{"does not match its checksum", "nothing was changed"}},
		{name: "no checksum line", checksums: func(string, string) string { return strings.Repeat("ab", 32) + "  other.tar.gz\n" },
			wantCode: exitError, wantVersion: installedVersion, wantErr: []string{"no checksum for the archive", "checksums.txt has no line for"}},
		{name: "another version", args: []string{"--version", "v2.2.0"}, wantCode: exitError, wantVersion: installedVersion,
			wantErr: []string{"another version than requested", "it reports " + offeredVersion + ", not v2.2.0", "nothing was changed"}},
		{name: "tools unavailable", setup: func(f *upgradeFixture) { f.binary.toolsErr = errors.New("rclone: not found") },
			wantCode: exitError, wantVersion: installedVersion, wantErr: []string{"tools are not available", "rclone: not found", "the worker was not changed"}},
		{name: "service not installed", setup: func(f *upgradeFixture) { f.services.state = service.StateNotInstalled },
			wantCode: exitError, wantVersion: installedVersion, wantErr: []string{"the worker service is not installed", "add --no-restart"}},
		{name: "stopped service is started", setup: func(f *upgradeFixture) { f.services.state = service.StateStopped },
			wantCode: exitOK, wantVersion: offeredVersion, wantCalls: []string{"restart"},
			wantOut: []string{"The syncr-worker service is stopped; the upgrade will start it"}},
		{name: "no rights", setup: func(f *upgradeFixture) {
			f.services.preflight = &service.PermissionError{Scope: service.ScopeSystem, Hint: "run it as root (for example with sudo)"}
		}, wantCode: exitError, wantVersion: installedVersion,
			wantErr: []string{"upgrading the worker of the system service needs elevated rights: run it as root", "nothing was changed"}},
		{name: "invalid version", args: []string{"--version", "2.1"}, wantCode: exitUsage, wantVersion: installedVersion,
			wantErr: []string{"syncr-worker upgrade: invalid version \"2.1\"", "v2.1.0-rc.1"}},
		{name: "extra argument", args: []string{"now"}, wantCode: exitUsage, wantVersion: installedVersion,
			wantErr: []string{"unexpected arguments"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			offer := offeredVersion
			if tt.offer != "" {
				offer = tt.offer
			}
			r := newRelease(t, offer, tt.checksums)
			code, stdout, stderr := f.run(t, append([]string{"--from", r.srv.URL, "--config", f.config}, tt.args...)...)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.wantCode, stdout, stderr)
			}
			if got := f.installed(t); got != tt.wantVersion {
				t.Errorf("installed %s, want %s", got, tt.wantVersion)
			}
			if got := f.services.changes(); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("service calls = %v, want %v", got, tt.wantCalls)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout)
				}
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if tt.wantCode != exitOK && !strings.HasPrefix(stderr, "syncr-worker upgrade") && tt.name != "extra argument" {
				t.Errorf("stderr does not name the command: %q", stderr)
			}
		})
	}
}

// TestUpgradeCommandRefusals: what is refused before anything is
// downloaded, with its exit status.
func TestUpgradeCommandRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     func(f *upgradeFixture) []string
		wantCode int
		wantErr  []string
	}{
		{"plain http to another machine", func(f *upgradeFixture) []string {
			return []string{"--from", "http://mirror.example.com/syncr", "--config", f.config}
		}, exitUsage, []string{"refusing to download from an insecure source", "--from must be an https URL"}},
		{"not a URL", func(f *upgradeFixture) []string { return []string{"--from", "mirror/syncr", "--config", f.config} },
			exitUsage, []string{"insecure source", "not an absolute URL"}},
		{"missing config", func(f *upgradeFixture) []string { return []string{"--config", filepath.Join(f.dir, "nope.yaml")} },
			exitError, []string{"the worker's config file", "is not readable", "pass --config", "needs --user here too"}},
		{"invalid config", func(f *upgradeFixture) []string {
			bad := filepath.Join(f.dir, "bad.yaml")
			if err := os.WriteFile(bad, []byte("max_concurrent_jobs: 0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", bad}
		}, exitError, []string{"max_concurrent_jobs"}},
		{"bad flag", func(*upgradeFixture) []string { return []string{"--latest"} }, exitUsage, []string{"flag provided but not defined: -latest"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			code, _, stderr := f.run(t, tt.args(f)...)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d: %s", code, tt.wantCode, stderr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if got := f.installed(t); got != installedVersion || len(f.services.changes()) != 0 {
				t.Errorf("installed %s, service calls %v", got, f.services.changes())
			}
		})
	}
}

// TestUpgradeCommandUserScope: --user upgrades the per-user service.
func TestUpgradeCommandUserScope(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	r := newRelease(t, offeredVersion, nil)
	if code, stdout, stderr := f.run(t, "--user", "--from", r.srv.URL+"/", "--config", f.config); code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if spec := f.services.specs[0]; spec.Scope != service.ScopeUser {
		t.Errorf("scope = %v, want the user service", spec.Scope)
	}
}

// TestUpgradeCommandNotWritable: a binary the user cannot replace (another
// user's install, a container image) is refused before anything changed.
func TestUpgradeCommandNotWritable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test cannot write: POSIX permissions, not root")
	}
	f := newUpgradeFixture(t)
	r := newRelease(t, offeredVersion, nil)
	bin := filepath.Dir(f.exe)
	if err := os.Chmod(bin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o755) })
	code, _, stderr := f.run(t, "--from", r.srv.URL, "--config", f.config)
	if code != exitError {
		t.Errorf("exit %d, want %d", code, exitError)
	}
	for _, want := range []string{"the worker binary cannot be replaced", "cannot write to " + bin, "pulling the new image"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if got := f.installed(t); got != installedVersion || len(f.services.changes()) != 0 {
		t.Errorf("installed %s, service calls %v", got, f.services.changes())
	}
}

// TestUpgradeCommandHelp: the help names every flag the controller's
// upgrade commands use, and the exit statuses.
func TestUpgradeCommandHelp(t *testing.T) {
	t.Parallel()
	e, _, stderr := testEnv(nil)
	if code := run(t.Context(), []string{"upgrade", "-h"}, e); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Usage: syncr-worker upgrade [--version vX.Y.Z] [--from URL] [--user] [--config PATH] [--no-restart] [--dry-run]",
		"-version", "-from", "-user", "-config", "-no-restart", "-dry-run", "Nothing on the machine changes before step 4", "Exit status: 0"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, stderr)
		}
	}
	e, _, stderr = testEnv(nil)
	if run(t.Context(), []string{"help"}, e); !strings.Contains(stderr.String(), "upgrade    replace this worker with another release") {
		t.Errorf("usage lacks the upgrade command:\n%s", stderr)
	}
}
