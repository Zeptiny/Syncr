package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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
)

const (
	oldVersion = "v2.0.0"
	newVersion = "v2.1.0"
	mirror     = "https://mirror.example/syncr/v2.1.0"
	logHint    = "journalctl -u syncr-worker -f"
)

// binaryFor is the content of a fake worker binary of version v; the fake
// Binary reads the version back from the file it is given.
func binaryFor(v string) []byte { return []byte(BinaryName + " " + v + " (fake)\n") }

// fakeHTTP serves files by URL and records the requests.
type fakeHTTP struct {
	mu       sync.Mutex
	files    map[string][]byte
	requests []string
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.URL.String())
	if req.Header.Get("User-Agent") == "" {
		return nil, errors.New("no User-Agent")
	}
	body, ok := f.files[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: req}, nil
}

// fakeBinary implements Binary over the fake binaries' content.
type fakeBinary struct {
	mu       sync.Mutex
	calls    []string
	paths    []string
	pins     []tools.ToolVersion
	fetchErr error
	checkErr error
	// configured is what CheckTools was given.
	configured map[string]string
}

func (b *fakeBinary) record(call, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
	b.paths = append(b.paths, path)
}

func (b *fakeBinary) Version(_ context.Context, path string) (string, error) {
	b.record("version", path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(raw))
	if len(f) < 2 || f[0] != BinaryName {
		return "", fmt.Errorf("not a worker binary: %q", raw)
	}
	return f[1], nil
}

func (b *fakeBinary) FetchTools(_ context.Context, path, toolsDir string) ([]tools.ToolVersion, error) {
	b.record("fetch "+toolsDir, path)
	return b.pins, b.fetchErr
}

func (b *fakeBinary) CheckTools(_ context.Context, path, toolsDir string, configured map[string]string) error {
	b.record("check "+toolsDir, path)
	b.mu.Lock()
	b.configured = configured
	b.mu.Unlock()
	return b.checkErr
}

func (b *fakeBinary) callList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// fakeServices implements Services. Status returns installed until the
// service is restarted, then afterRestart(poll) for each poll.
type fakeServices struct {
	mu           sync.Mutex
	exe          string
	calls        []string
	preflightErr error
	installed    service.State
	afterRestart func(poll int) (service.State, error)
	restartErr   error
	onRestart    func()
	polls        int
	restarted    bool
	// ran holds the installed binary's content at each Restart and Start.
	ran []string
	// program and programErr are what InstalledExecutable returns; an empty
	// program means exe.
	program    string
	programErr error
}

func (s *fakeServices) call(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *fakeServices) Preflight(service.Spec) error { s.call("preflight"); return s.preflightErr }

func (s *fakeServices) Status(context.Context, service.Spec) (service.State, error) {
	s.call("status")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.restarted {
		return s.installed, nil
	}
	s.polls++
	if s.afterRestart == nil {
		return service.StateRunning, nil
	}
	return s.afterRestart(s.polls)
}

func (s *fakeServices) runs() {
	raw, _ := os.ReadFile(s.exe)
	s.mu.Lock()
	s.ran = append(s.ran, string(raw))
	s.mu.Unlock()
}

func (s *fakeServices) Restart(context.Context, service.Spec) error {
	s.call("restart")
	if s.onRestart != nil {
		s.onRestart()
	}
	s.runs()
	s.mu.Lock()
	s.restarted = true
	s.mu.Unlock()
	return s.restartErr
}

func (s *fakeServices) Stop(context.Context, service.Spec) error { s.call("stop"); return nil }

func (s *fakeServices) Start(context.Context, service.Spec) error {
	s.call("start")
	s.runs()
	return nil
}

func (s *fakeServices) InstalledExecutable(context.Context, service.Spec) (string, error) {
	s.call("executable")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.programErr != nil {
		return "", s.programErr
	}
	return cmp.Or(s.program, s.exe), nil
}

func (s *fakeServices) Info(service.Spec) service.Info { return service.Info{LogHint: logHint} }

func (s *fakeServices) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// fakePruner records what it was asked to keep.
type fakePruner struct {
	mu   sync.Mutex
	keep [][]tools.ToolVersion
	err  error
}

func (p *fakePruner) Prune(_ context.Context, keep []tools.ToolVersion) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keep = append(p.keep, keep)
	return []string{"/tools/rclone/1.75.2"}, p.err
}

type fixture struct {
	dir, exe string
	http     *fakeHTTP
	bin      *fakeBinary
	svc      *fakeServices
	pruner   *fakePruner
	out      *bytes.Buffer
	sleeps   *[]time.Duration
	cfg      Config
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range []struct {
		name string
		body []byte
	}{{"README.md", []byte("readme")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(m.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// newFixture installs a fake v2.0.0 worker in a temporary directory and
// serves a v2.1.0 release for linux/amd64 at mirror.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	// The upgrade resolves the binary's path (filepath.EvalSymlinks), and so
	// works in the canonical form of the directory: on Windows a temporary
	// directory may be an 8.3 short path (C:\Users\RUNNER~1\...) that
	// resolves to the long one.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(exe, binaryFor(oldVersion), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := tarGz(t, BinaryName, binaryFor(newVersion))
	const asset = "syncr-worker_linux_amd64.tar.gz"
	checksums := fmt.Sprintf("%s  syncr-controller_linux_amd64.tar.gz\n%s  %s\n%s  syncr-worker_windows_amd64.zip\n",
		sha([]byte("other")), sha(archive), asset, sha([]byte("zip")))
	h := &fakeHTTP{files: map[string][]byte{
		mirror + "/" + ChecksumsFile: []byte(checksums),
		mirror + "/" + asset:         archive,
	}}
	f := &fixture{
		dir: dir, exe: exe, http: h,
		bin:    &fakeBinary{pins: []tools.ToolVersion{{Tool: "rclone", Version: "1.76.0"}, {Tool: "restic", Version: "0.20.0"}}},
		svc:    &fakeServices{exe: exe, installed: service.StateRunning},
		pruner: &fakePruner{},
		out:    &bytes.Buffer{},
		sleeps: &[]time.Duration{},
	}
	f.cfg = Config{
		Version: newVersion, From: mirror + "/", Executable: exe, Current: oldVersion,
		OS: "linux", Arch: "amd64",
		ToolsDir:  filepath.Join(t.TempDir(), "tools"),
		ToolPaths: map[string]string{"rclone": "/opt/rclone", "restic": ""},
		Service:   service.Spec{Name: service.Name},
		Out:       f.out,
		HTTP:      h, Services: f.svc, Binary: f.bin, Swapper: OSSwapper{}, Pruner: f.pruner,
		Sleep: func(_ context.Context, d time.Duration) error {
			*f.sleeps = append(*f.sleeps, d)
			return nil
		},
	}
	return f
}

func (f *fixture) installed(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertUnchanged checks that the machine is as before the upgrade: the old
// binary in place, nothing else in its directory, the service untouched.
func (f *fixture) assertUnchanged(t *testing.T) {
	t.Helper()
	if got := f.installed(t); got != string(binaryFor(oldVersion)) {
		t.Errorf("installed binary = %q, want the old one", got)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != BinaryName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("files next to the binary = %q, want only %s", names, BinaryName)
	}
	for _, c := range f.svc.callList() {
		if c != "preflight" && c != "status" && c != "executable" {
			t.Errorf("the service was changed: %v", f.svc.callList())
			break
		}
	}
	if len(f.pruner.keep) != 0 {
		t.Error("tools were pruned")
	}
}

func TestUpgradeSucceeds(t *testing.T) {
	t.Parallel()
	for _, windows := range []bool{false, true} {
		t.Run(fmt.Sprintf("windows swap %v", windows), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.cfg.Swapper = OSSwapper{Windows: windows}
			res, err := Run(t.Context(), f.cfg)
			if err != nil {
				t.Fatalf("Run: %v\n%s", err, f.out)
			}
			if res.Old != oldVersion || res.New != newVersion || !res.Swapped || !res.Restarted || res.UpToDate || res.DryRun || len(res.Warnings) != 0 {
				t.Errorf("result = %+v", res)
			}
			if got := f.installed(t); got != string(binaryFor(newVersion)) {
				t.Errorf("installed binary = %q", got)
			}
			if runtime.GOOS != "windows" {
				if fi, _ := os.Stat(f.exe); fi.Mode().Perm() != 0o755 {
					t.Errorf("mode = %o, want 755", fi.Mode().Perm())
				}
			}
			// Nothing left behind: no backup, no temporary file.
			if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
				t.Errorf("directory holds %d entries, want only the binary", len(entries))
			}
			// The service restarted on the new binary, then was watched once a
			// second for the health window.
			if want := []string{"preflight", "status", "executable", "restart"}; !slices.Equal(f.svc.callList()[:4], want) {
				t.Errorf("service calls = %v", f.svc.callList())
			}
			if f.svc.polls != 20 || len(*f.sleeps) != 20 || (*f.sleeps)[0] != time.Second {
				t.Errorf("health check: %d polls, sleeps %v", f.svc.polls, *f.sleeps)
			}
			if len(f.svc.ran) != 1 || f.svc.ran[0] != string(binaryFor(newVersion)) {
				t.Errorf("the service restarted on %q", f.svc.ran)
			}
			// The new binary ran from a temporary file next to the installed
			// one, its tools were fetched and checked before the swap, and the
			// old versions pruned after the health check.
			if got, want := f.bin.callList(), []string{"version", "fetch " + f.cfg.ToolsDir, "check " + f.cfg.ToolsDir}; !slices.Equal(got, want) {
				t.Errorf("binary calls = %v, want %v", got, want)
			}
			for _, p := range f.bin.paths {
				if filepath.Dir(p) != f.dir || !strings.HasPrefix(filepath.Base(p), ".syncr-worker-new-") {
					t.Errorf("new binary ran from %s", p)
				}
			}
			if f.bin.configured["rclone"] != "/opt/rclone" {
				t.Errorf("configured tools = %v", f.bin.configured)
			}
			if len(f.pruner.keep) != 1 || !slices.Equal(f.pruner.keep[0], f.bin.pins) || !slices.Equal(res.Pruned, []string{"/tools/rclone/1.75.2"}) {
				t.Errorf("pruned with %v, result %v", f.pruner.keep, res.Pruned)
			}
			if !strings.Contains(f.out.String(), "Upgraded from v2.0.0 to v2.1.0") {
				t.Errorf("output:\n%s", f.out)
			}
			if got := f.http.requests; len(got) != 2 || got[0] != mirror+"/checksums.txt" || got[1] != mirror+"/syncr-worker_linux_amd64.tar.gz" {
				t.Errorf("requests = %q", got)
			}
		})
	}
}

func TestUpgradeRefusesBeforeChangingAnything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		want   error
		// permission expects a *service.PermissionError instead.
		permission bool
		// requests is the number of HTTP requests expected (-1: any).
		requests int
	}{
		{
			name: "tampered archive",
			mutate: func(t *testing.T, f *fixture) {
				u := mirror + "/syncr-worker_linux_amd64.tar.gz"
				f.http.files[u] = tarGz(t, BinaryName, binaryFor("v6.6.6"))
			},
			want: ErrChecksumMismatch, requests: 2,
		},
		{
			name: "no checksum line",
			mutate: func(_ *testing.T, f *fixture) {
				f.http.files[mirror+"/checksums.txt"] = []byte(sha([]byte("x")) + "  syncr-worker_linux_arm64.tar.gz\n")
			},
			want: ErrNoChecksum, requests: 1,
		},
		{
			name: "malformed checksum line",
			mutate: func(_ *testing.T, f *fixture) {
				f.http.files[mirror+"/checksums.txt"] = []byte("abc  syncr-worker_linux_amd64.tar.gz\n")
			},
			want: ErrNoChecksum, requests: 1,
		},
		{name: "wrong version", mutate: func(_ *testing.T, f *fixture) { f.cfg.Version = "v2.2.0" }, want: ErrVersionMismatch, requests: -1},
		{name: "invalid version", mutate: func(_ *testing.T, f *fixture) { f.cfg.Version = "v2.1.0/../../x" }, want: ErrInvalidVersion, requests: 0},
		{name: "version without v", mutate: func(_ *testing.T, f *fixture) { f.cfg.Version = "2.1.0" }, want: ErrInvalidVersion, requests: 0},
		{name: "http mirror", mutate: func(_ *testing.T, f *fixture) { f.cfg.From = "http://mirror.example/syncr" }, want: ErrInsecureSource, requests: 0},
		{name: "ftp mirror", mutate: func(_ *testing.T, f *fixture) { f.cfg.From = "ftp://127.0.0.1/syncr" }, want: ErrInsecureSource, requests: 0},
		{name: "mirror with a query", mutate: func(_ *testing.T, f *fixture) { f.cfg.From = mirror + "?x=1" }, want: ErrInsecureSource, requests: 0},
		{name: "service not installed", mutate: func(_ *testing.T, f *fixture) { f.svc.installed = service.StateNotInstalled }, want: ErrServiceNotInstalled, requests: 0},
		{
			name:       "no rights for the service",
			mutate:     func(_ *testing.T, f *fixture) { f.svc.preflightErr = &service.PermissionError{Hint: "run it as root"} },
			permission: true, requests: 0,
		},
		{name: "tools cannot be fetched", mutate: func(_ *testing.T, f *fixture) { f.bin.fetchErr = errors.New("download refused") }, want: ErrToolsUnavailable, requests: 2},
		{name: "tools do not resolve", mutate: func(_ *testing.T, f *fixture) { f.bin.checkErr = errors.New("rclone missing") }, want: ErrToolsUnavailable, requests: 2},
		{
			name: "archive without the binary",
			mutate: func(t *testing.T, f *fixture) {
				archive := tarGz(t, "syncr-controller", binaryFor(newVersion))
				f.http.files[mirror+"/syncr-worker_linux_amd64.tar.gz"] = archive
				f.http.files[mirror+"/checksums.txt"] = []byte(sha(archive) + "  syncr-worker_linux_amd64.tar.gz\n")
			},
			want: nil, requests: 2,
		},
		{name: "release missing", mutate: func(_ *testing.T, f *fixture) { f.cfg.From = "https://mirror.example/none" }, want: nil, requests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tt.mutate(t, f)
			_, err := Run(t.Context(), f.cfg)
			if err == nil {
				t.Fatalf("Run succeeded\n%s", f.out)
			}
			if tt.permission {
				if _, ok := errors.AsType[*service.PermissionError](err); !ok {
					t.Errorf("err = %v, want a *service.PermissionError", err)
				}
			} else if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.requests >= 0 && len(f.http.requests) != tt.requests {
				t.Errorf("%d requests %q, want %d", len(f.http.requests), f.http.requests, tt.requests)
			}
			f.assertUnchanged(t)
		})
	}
}

func TestUpgradeToTheInstalledVersionIsANoOp(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Current = newVersion
	res, err := Run(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.UpToDate || res.Swapped || res.New != newVersion {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(f.out.String(), "already installed") {
		t.Errorf("output:\n%s", f.out)
	}
	if got := f.bin.callList(); !slices.Equal(got, []string{"version"}) {
		t.Errorf("binary calls = %v", got)
	}
	// Still the fixture's installed content: the "new" release is the same
	// version, and nothing was swapped.
	f.assertUnchanged(t)
}

func TestUpgradeRollsBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(f *fixture)
		cause  string
	}{
		{
			name: "service crashes",
			mutate: func(f *fixture) {
				f.svc.afterRestart = func(poll int) (service.State, error) {
					if poll >= 3 {
						return service.StateFailing, nil
					}
					return service.StateRunning, nil
				}
			},
			cause: "failing",
		},
		{
			name: "service stops",
			mutate: func(f *fixture) {
				f.svc.afterRestart = func(int) (service.State, error) { return service.StateStopped, nil }
			},
			cause: "stopped",
		},
		{name: "restart fails", mutate: func(f *fixture) { f.svc.restartErr = errors.New("unit failed") }, cause: "unit failed"},
		{
			name: "status fails",
			mutate: func(f *fixture) {
				f.svc.afterRestart = func(int) (service.State, error) { return service.StateFailing, errors.New("bus gone") }
			},
			cause: "bus gone",
		},
	}
	for _, tt := range tests {
		for _, windows := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, windows swap %v", tt.name, windows), func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.cfg.Swapper = OSSwapper{Windows: windows}
				tt.mutate(f)
				res, err := Run(t.Context(), f.cfg)
				if !errors.Is(err, ErrRolledBack) {
					t.Fatalf("err = %v, want ErrRolledBack\n%s", err, f.out)
				}
				for _, want := range []string{tt.cause, logHint, oldVersion, newVersion} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				if res.Swapped || res.Restarted || len(f.pruner.keep) != 0 {
					t.Errorf("result = %+v, pruned %v", res, f.pruner.keep)
				}
				// Stopped, old binary back, started again on it.
				calls := f.svc.callList()
				if n := len(calls); n < 2 || calls[n-2] != "stop" || calls[n-1] != "start" {
					t.Errorf("service calls = %v, want stop and start at the end", calls)
				}
				if len(f.svc.ran) != 2 || f.svc.ran[0] != string(binaryFor(newVersion)) || f.svc.ran[1] != string(binaryFor(oldVersion)) {
					t.Errorf("binaries run: %q", f.svc.ran)
				}
				if got := f.installed(t); got != string(binaryFor(oldVersion)) {
					t.Errorf("installed binary = %q", got)
				}
				if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
					t.Errorf("directory holds %d entries after the rollback, want only the binary", len(entries))
				}
			})
		}
	}
}

// brokenRestore is a Swapper whose Restore fails.
type brokenRestore struct{ OSSwapper }

func (brokenRestore) Restore(string, string) error { return errors.New("disk full") }

func TestUpgradeRollbackFailureSaysWhatToDo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Swapper = brokenRestore{}
	f.svc.afterRestart = func(int) (service.State, error) { return service.StateFailing, nil }
	_, err := Run(t.Context(), f.cfg)
	if !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("err = %v, want ErrRollbackFailed", err)
	}
	for _, want := range []string{f.exe + BackupSuffix, f.exe, "disk full", logHint} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The old binary is still there to be moved back by hand.
	if raw, err := os.ReadFile(f.exe + BackupSuffix); err != nil || string(raw) != string(binaryFor(oldVersion)) {
		t.Errorf("backup = %q, %v", raw, err)
	}
}

func TestUpgradeFinishesWhenInterruptedAfterTheSwap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.svc.onRestart = cancel // Ctrl-C during the restart
	f.cfg.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	res, err := Run(ctx, f.cfg)
	if err != nil || !res.Restarted {
		t.Fatalf("Run = %+v, %v; want the upgrade finished", res, err)
	}
	if got := f.installed(t); got != string(binaryFor(newVersion)) {
		t.Errorf("installed binary = %q", got)
	}
}

func TestUpgradeWithoutRestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.NoRestart = true
	f.cfg.Services = nil
	res, err := Run(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Swapped || res.Restarted || len(res.Pruned) != 0 || len(f.pruner.keep) != 0 {
		t.Errorf("result = %+v; the old tools must stay for the running worker", res)
	}
	if got := f.installed(t); got != string(binaryFor(newVersion)) {
		t.Errorf("installed binary = %q", got)
	}
	if len(f.svc.callList()) != 0 {
		t.Errorf("service calls = %v", f.svc.callList())
	}
	if !strings.Contains(f.out.String(), "Restart the worker") {
		t.Errorf("output:\n%s", f.out)
	}
}

func TestUpgradeDryRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.DryRun = true
	res, err := Run(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Swapped || res.New != newVersion {
		t.Errorf("result = %+v", res)
	}
	if got := f.bin.callList(); !slices.Equal(got, []string{"version"}) {
		t.Errorf("binary calls = %v", got)
	}
	if !strings.Contains(f.out.String(), "Nothing was changed") {
		t.Errorf("output:\n%s", f.out)
	}
	f.assertUnchanged(t)
}

func TestUpgradeWindowsRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	archive := zipOf(t, "syncr-worker.exe", binaryFor(newVersion))
	f.http.files[mirror+"/syncr-worker_windows_amd64.zip"] = archive
	f.http.files[mirror+"/checksums.txt"] = []byte(sha(archive) + "  syncr-worker_windows_amd64.zip\n")
	f.cfg.OS = "windows"
	f.cfg.NoRestart = true
	if _, err := Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run: %v\n%s", err, f.out)
	}
	if got := f.installed(t); got != string(binaryFor(newVersion)) {
		t.Errorf("installed binary = %q", got)
	}
	// The new binary is run from a file named like an executable.
	if len(f.bin.paths) == 0 || !strings.HasSuffix(f.bin.paths[0], ".exe") {
		t.Errorf("new binary ran from %q", f.bin.paths)
	}
}

func TestUpgradeRefusesAnUnwritableDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a Unix directory this user cannot write")
	}
	f := newFixture(t)
	if err := os.Chmod(f.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o755) })
	_, err := Run(t.Context(), f.cfg)
	if !errors.Is(err, ErrNotWritable) || !strings.Contains(err.Error(), "container") {
		t.Fatalf("err = %v, want ErrNotWritable with advice", err)
	}
	f.assertUnchanged(t)
}

func TestReleaseBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		from, version, want string
		ok                  bool
	}{
		{"", "v2.1.0", "https://github.com/Zeptiny/Syncr/releases/download/v2.1.0", true},
		{"", "", "https://github.com/Zeptiny/Syncr/releases/latest/download", true},
		{"https://mirror.example/a/", "v2.1.0", "https://mirror.example/a", true},
		{"http://127.0.0.1:8000/rel", "", "http://127.0.0.1:8000/rel", true},
		{"http://[::1]:8000", "", "http://[::1]:8000", true},
		{"http://localhost:8000/x", "", "http://localhost:8000/x", true},
		{"http://127.1.2.3/x", "", "http://127.1.2.3/x", true},
		{"http://mirror.example/a", "", "", false},
		{"http://10.0.0.1/a", "", "", false},
		{"https:///no-host", "", "", false},
		{"mirror.example/a", "", "", false},
		{"https://mirror.example/a#frag", "", "", false},
	}
	for _, tt := range tests {
		got, err := releaseBase(tt.from, tt.version)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("releaseBase(%q, %q) = %q, %v; want %q (ok %v)", tt.from, tt.version, got, err, tt.want, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrInsecureSource) {
			t.Errorf("releaseBase(%q): %v is not ErrInsecureSource", tt.from, err)
		}
	}
}

func TestDefaultDownloadURLs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ version, prefix string }{
		{"v2.1.0", DefaultReleases + "/download/v2.1.0/"},
		{"", DefaultReleases + "/latest/download/"},
	} {
		f := newFixture(t)
		f.cfg.From, f.cfg.Version = "", tt.version
		_, _ = Run(t.Context(), f.cfg) // 404: only the URL matters
		if len(f.http.requests) != 1 || f.http.requests[0] != tt.prefix+ChecksumsFile {
			t.Errorf("version %q: requests = %q", tt.version, f.http.requests)
		}
	}
}

func TestChecksumOf(t *testing.T) {
	t.Parallel()
	good := sha([]byte("a"))
	tests := []struct {
		name, file string
		want       string
		ok         bool
	}{
		{"goreleaser format", good + "  syncr-worker_linux_amd64.tar.gz\n", good, true},
		{"binary marker", good + " *syncr-worker_linux_amd64.tar.gz\n", good, true},
		{"upper case", strings.ToUpper(good) + "  syncr-worker_linux_amd64.tar.gz\n", good, true},
		{"same line twice", good + "  syncr-worker_linux_amd64.tar.gz\n" + good + "  syncr-worker_linux_amd64.tar.gz\n", good, true},
		{"conflicting lines", good + "  syncr-worker_linux_amd64.tar.gz\n" + sha([]byte("b")) + "  syncr-worker_linux_amd64.tar.gz\n", "", false},
		{"prefix of another name", good + "  syncr-worker_linux_amd64.tar.gz.sig\n", "", false},
		{"not hex", strings.Repeat("z", 64) + "  syncr-worker_linux_amd64.tar.gz\n", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		got, err := checksumOf([]byte(tt.file), "syncr-worker_linux_amd64.tar.gz")
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("%s: checksumOf = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestUpgradeAsksTheInstalledBinaryItsVersion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Current = ""
	f.cfg.Service.Executable = f.exe
	res, err := Run(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Old != oldVersion || res.New != newVersion {
		t.Errorf("result = %+v", res)
	}
	if len(f.bin.paths) == 0 || f.bin.paths[0] != f.exe || f.bin.calls[0] != "version" {
		t.Errorf("first binary call: %v on %v, want version on the installed binary", f.bin.calls, f.bin.paths)
	}
}

func TestUpgradeStartsAStoppedService(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.svc.installed = service.StateStopped
	if _, err := Run(t.Context(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "is stopped; the upgrade will start it") {
		t.Errorf("output:\n%s", f.out)
	}
}

func TestNewUpgraderValidates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for name, mutate := range map[string]func(*Config){
		"relative executable": func(c *Config) { c.Executable = "syncr-worker" },
		"missing executable":  func(c *Config) { c.Executable = filepath.Join(f.dir, "nope") },
		"service runs another binary": func(c *Config) {
			other := filepath.Join(f.dir, "other")
			if err := os.WriteFile(other, nil, 0o755); err != nil {
				t.Fatal(err)
			}
			c.Service.Executable = other
		},
		"no tools directory": func(c *Config) { c.ToolsDir = "" },
		"no service manager": func(c *Config) { c.Services = nil },
	} {
		cfg := f.cfg
		mutate(&cfg)
		if _, err := Run(t.Context(), cfg); err == nil {
			t.Errorf("%s: Run succeeded", name)
		}
	}
	if len(f.http.requests) != 0 {
		t.Errorf("requests = %q", f.http.requests)
	}
}

// TestUpgradeRefusesAnotherServiceBinary: the service must run the binary
// the upgrade replaces. Another copy of the worker is refused before
// anything is downloaded or changed, naming the binary to run instead.
func TestUpgradeRefusesAnotherServiceBinary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// program returns what the service runs, given the fixture.
		program func(t *testing.T, f *fixture) string
		refused bool
	}{
		{"the same file", func(_ *testing.T, f *fixture) string { return f.exe }, false},
		{"the same file through another path", func(_ *testing.T, f *fixture) string {
			return filepath.Join(f.dir, ".", "sub", "..", BinaryName)
		}, false},
		{"a symbolic link to it", func(t *testing.T, f *fixture) string {
			if runtime.GOOS == "windows" {
				t.Skip("symbolic links need a privilege on Windows")
			}
			link := filepath.Join(t.TempDir(), "worker-link")
			if err := os.Symlink(f.exe, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, false},
		{"another copy", func(t *testing.T, _ *fixture) string {
			other := filepath.Join(t.TempDir(), BinaryName)
			if err := os.WriteFile(other, binaryFor(oldVersion), 0o755); err != nil {
				t.Fatal(err)
			}
			return other
		}, true},
		{"a hard link in another directory", func(t *testing.T, f *fixture) string {
			// The same file, but replacing f.exe would not replace it.
			other := filepath.Join(f.dir, "elsewhere", BinaryName)
			if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(f.exe, other); err != nil {
				t.Skipf("no hard links here: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(other)) })
			return other
		}, true},
		{"a file that is gone", func(t *testing.T, _ *fixture) string { return filepath.Join(t.TempDir(), BinaryName) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if err := os.MkdirAll(filepath.Join(f.dir, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			f.svc.program = tt.program(t, f)
			_, err := Run(t.Context(), f.cfg)
			if !tt.refused {
				if err != nil {
					t.Fatalf("Run: %v\n%s", err, f.out)
				}
				return
			}
			if !errors.Is(err, ErrOtherBinary) {
				t.Fatalf("err = %v, want ErrOtherBinary", err)
			}
			for _, want := range []string{f.svc.program, f.exe, "nothing was changed", f.svc.program + " upgrade"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if len(f.http.requests) != 0 {
				t.Errorf("downloaded %q before refusing", f.http.requests)
			}
			if got := f.svc.callList(); !slices.Equal(got, []string{"preflight", "status", "executable"}) {
				t.Errorf("service calls = %v", got)
			}
			if got := f.installed(t); got != string(binaryFor(oldVersion)) {
				t.Errorf("installed binary = %q", got)
			}
		})
	}
}

func TestUpgradeServiceBinaryUnreadable(t *testing.T) {
	t.Parallel()
	// Where the service manager's definition cannot be read, the upgrade
	// goes on and says what it could not check.
	f := newFixture(t)
	f.svc.programErr = fmt.Errorf("%w: sysv", service.ErrUnsupported)
	if _, err := Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(f.out.String(), "cannot be read on this system; make sure it is "+f.exe) {
		t.Errorf("output:\n%s", f.out)
	}

	// Any other failure to read it refuses the upgrade.
	f = newFixture(t)
	f.svc.programErr = errors.New("systemctl show failed")
	_, err := Run(t.Context(), f.cfg)
	if err == nil || !strings.Contains(err.Error(), "systemctl show failed") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if len(f.http.requests) != 0 {
		t.Errorf("downloaded %q", f.http.requests)
	}

	// Without a restart no service is asked.
	f = newFixture(t)
	f.cfg.NoRestart = true
	f.svc.programErr = errors.New("must not be asked")
	if _, err := Run(t.Context(), f.cfg); err != nil {
		t.Fatalf("Run --no-restart: %v", err)
	}
	if len(f.svc.callList()) != 0 {
		t.Errorf("service calls = %v", f.svc.callList())
	}
}
