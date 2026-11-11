package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Zeptiny/Syncr/internal/link"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// The test binary doubles as a child process that prints one long line, to
// check the supervisor limit that buildWorker configures. The variable
// reaches the child through proc.Spec.Env.
const helperLineEnv = "SYNCR_WORKER_TEST_LINE_BYTES"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(helperLineEnv); ok {
		n, _ := strconv.Atoi(v)
		fmt.Println(strings.Repeat("x", n))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeVersions stands in for running "<tool> version": it knows the output
// of specific binaries by path and fails for every other path, so tests never
// depend on the rclone or restic installed on the machine.
type fakeVersions struct {
	mu     sync.Mutex
	output map[string]string // cleaned absolute path -> version output
	ran    []string
}

func (f *fakeVersions) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	f.mu.Lock()
	f.ran = append(f.ran, spec.Path)
	out, ok := f.output[filepath.Clean(spec.Path)]
	f.mu.Unlock()
	if !ok {
		return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
	}
	spec.Stdout(proc.Line{Data: []byte(out)})
	return proc.Result{}, nil
}

// pinnedOutput returns what the pinned version of tool prints.
func pinnedOutput(t *testing.T, tool string) string {
	t.Helper()
	spec, ok := tools.DefaultManifest().Lookup(tool)
	if !ok {
		t.Fatalf("manifest does not pin %s", tool)
	}
	if tool == toolRclone {
		return "rclone v" + spec.Version
	}
	return "restic " + spec.Version + " compiled with go1.x on " + runtime.GOOS + "/" + runtime.GOARCH
}

// fakeToolFile creates an executable-looking file for tool in dir and
// registers its version output.
func (f *fakeVersions) fakeToolFile(t *testing.T, dir, tool, output string) string {
	t.Helper()
	file := tools.Spec{Name: tool}.Binary(runtime.GOOS)
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.output == nil {
		f.output = map[string]string{}
	}
	f.output[filepath.Clean(path)] = output
	return path
}

// fixture is a configured worker in a temporary directory with fake tools.
type fixture struct {
	t        *testing.T
	base     string
	cfg      workerconfig.Config
	versions *fakeVersions
	deps     workerDeps
	logs     *bytes.Buffer
	log      *slog.Logger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{t: t, base: base, versions: &fakeVersions{}, logs: &bytes.Buffer{}}
	f.log = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.cfg = workerconfig.Default()
	f.cfg.DataDir = filepath.Join(base, "data")
	f.cfg.LocalRoots = []string{filepath.Join(base, "roots")}
	f.cfg.Tools.RclonePath = f.versions.fakeToolFile(t, filepath.Join(base, "bin"), toolRclone, pinnedOutput(t, toolRclone))
	f.cfg.Tools.ResticPath = f.versions.fakeToolFile(t, filepath.Join(base, "bin"), toolRestic, pinnedOutput(t, toolRestic))
	if err := os.MkdirAll(f.cfg.LocalRoots[0], 0o755); err != nil {
		t.Fatal(err)
	}
	f.deps = workerDeps{
		paths: paths.Env{GOOS: runtime.GOOS, LookupEnv: func(string) (string, bool) { return "", false },
			UserHomeDir: func() (string, error) { return "", errors.New("no home in tests") }, Geteuid: func() int { return 1000 }},
		tools: toolsOverrides{runner: f.versions},
	}
	return f
}

func (f *fixture) build() (*worker, error) {
	f.t.Helper()
	return buildWorker(f.t.Context(), f.cfg, f.deps, f.log)
}

// servedProbeOps are the probe ops of the contract this worker answers:
// all but sftp_host_keys, whose prober does not exist yet (remove the
// exclusion with it).
func servedProbeOps() []string {
	return slices.DeleteFunc(link.ProbeOps(), func(op string) bool { return op == "sftp_host_keys" })
}

func TestBuildWorkerRegistersEveryRunner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"restic_backup", "restic_check", "restic_delete_data", "restic_forget", "restic_init",
		"restic_prune", "restic_restore", "restic_snapshots", "rclone_transfer",
	}
	slices.Sort(want)
	if got := w.registry.Kinds(); !slices.Equal(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}

	// The probe executor runs every probe operation with the same tools.
	if w.probes == nil || !slices.Equal(w.probes.Ops(), servedProbeOps()) {
		t.Errorf("probes = %v, want every operation %v", w.probes, servedProbeOps())
	}

	for _, tool := range []string{toolRclone, toolRestic} {
		res := w.tools[tool]
		if res.Source != tools.SourceConfigured || res.Path == "" {
			t.Errorf("%s resolved as %+v, want the configured binary", tool, res)
		}
	}
	for _, line := range []string{
		"tool resolved", "tool=rclone", "tool=restic", "source=configured", "version=",
		"worker directories", "local roots",
	} {
		if !strings.Contains(f.logs.String(), line) {
			t.Errorf("logs lack %q:\n%s", line, f.logs)
		}
	}
}

func TestBuildWorkerCreatesPrivateDirectoriesUnderTheDataDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if w.dataDir != f.cfg.DataDir || w.scratchDir != paths.ScratchDir(f.cfg.DataDir) || w.cacheDir != paths.CacheDir(f.cfg.DataDir) {
		t.Errorf("dirs = %q %q %q", w.dataDir, w.scratchDir, w.cacheDir)
	}
	for _, dir := range []string{w.scratchDir, filepath.Join(w.cacheDir, "restic")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s: %v", dir, err)
			continue
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v, want 0700", dir, info.Mode().Perm())
		}
	}
}

func TestBuildWorkerUsesPlatformDataDirByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.DataDir = ""
	home := filepath.Join(f.base, "home")
	f.deps.paths.UserHomeDir = func() (string, error) { return home, nil }
	f.deps.paths.LookupEnv = func(k string) (string, bool) {
		switch k {
		case "XDG_DATA_HOME":
			return filepath.Join(home, "xdg"), true
		case "LocalAppData", "ProgramData":
			return filepath.Join(home, "appdata"), true
		}
		return "", false
	}
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.dataDir, home) {
		t.Errorf("dataDir = %q, want it under %q (the platform default)", w.dataDir, home)
	}
}

func TestBuildWorkerReportsUnusableDataDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	blocker := filepath.Join(f.base, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg.DataDir = filepath.Join(blocker, "data") // below a regular file
	_, err := f.build()
	if err == nil || !strings.Contains(err.Error(), "SYNCR_DATA_DIR") {
		t.Fatalf("err = %v, want an actionable data directory error", err)
	}

	f = newFixture(t)
	f.cfg.DataDir = ""
	f.deps.paths.Geteuid = func() int { return 1000 } // user mode, but no home directory
	if _, err := f.build(); err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Errorf("err = %v, want advice to set data_dir", err)
	}
}

func TestBuildWorkerAppliesLocalRoots(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(f.cfg.LocalRoots[0], "data")
	if _, err := w.policy.Resolve(inside); err != nil {
		t.Errorf("Resolve(%s) = %v, want allowed", inside, err)
	}
	outside := filepath.Join(f.base, "elsewhere")
	if _, err := w.policy.Resolve(outside); !errors.Is(err, localfs.ErrNotAllowed) {
		t.Errorf("Resolve(%s) = %v, want ErrNotAllowed", outside, err)
	}
	if strings.Contains(f.logs.String(), "no local_roots") {
		t.Errorf("unexpected warning with roots configured:\n%s", f.logs)
	}
}

func TestBuildWorkerWithoutLocalRootsDeniesEverythingAndWarns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.LocalRoots = nil
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.policy.Resolve(filepath.Join(f.base, "anything")); !errors.Is(err, localfs.ErrNotAllowed) {
		t.Errorf("Resolve = %v, want ErrNotAllowed: an empty list denies all local access", err)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "no local_roots configured") ||
		!strings.Contains(logs, "SYNCR_LOCAL_ROOTS") {
		t.Errorf("logs lack the local_roots warning:\n%s", logs)
	}
	if len(w.registry.Kinds()) != 9 {
		t.Errorf("Kinds() = %v: runners must be registered even without local roots (remote-to-remote jobs)", w.registry.Kinds())
	}
}

func TestBuildWorkerRejectsInvalidLocalRoot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.LocalRoots = []string{"relative/path"}
	if _, err := f.build(); err == nil || !strings.Contains(err.Error(), "local_roots") {
		t.Errorf("err = %v, want a local_roots error", err)
	}
}

func TestBuildWorkerLogsRejectedCandidates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// The configured rclone has the wrong version; a correct one is bundled.
	wrong := f.versions.fakeToolFile(t, filepath.Join(f.base, "old"), toolRclone, "rclone v0.0.1")
	f.cfg.Tools.RclonePath = wrong
	bundled := filepath.Join(f.base, "bundled")
	want := f.versions.fakeToolFile(t, bundled, toolRclone, pinnedOutput(t, toolRclone))
	f.deps.bundledDir = bundled

	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	got := w.tools[toolRclone]
	if got.Source != tools.SourceBundled || got.Path != want {
		t.Errorf("rclone = %+v, want the bundled binary %s", got, want)
	}
	logs := f.logs.String()
	for _, line := range []string{"tool candidate rejected", "source=configured", "0.0.1", "want pinned"} {
		if !strings.Contains(logs, line) {
			t.Errorf("logs lack %q:\n%s", line, logs)
		}
	}
}

func TestBuildWorkerMissingToolsIsActionable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Tools = workerconfig.ToolsConfig{} // nothing configured, nothing installed
	_, err := f.build()
	if err == nil {
		t.Fatal("buildWorker succeeded without tools")
	}
	toolsErr, ok := errors.AsType[*toolsUnavailableError](err)
	if !ok {
		t.Fatalf("err = %T %v, want *toolsUnavailableError", err, err)
	}
	if want := filepath.Join(f.cfg.DataDir, "tools"); toolsErr.toolsDir != want {
		t.Errorf("toolsDir = %q, want %q", toolsErr.toolsDir, want)
	}
	msg := err.Error()
	for _, want := range []string{
		"rclone:", "restic:", // both missing tools are reported at once
		"this release pins rclone ", "restic ",
		"tools fetch --dir " + filepath.Join(f.cfg.DataDir, "tools"),
		"tools.download: true", "SYNCR_TOOLS_DOWNLOAD",
		"SYNCR_RCLONE_PATH", "SYNCR_RESTIC_PATH",
		"tools status",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
}

func TestBuildWorkerReportsAnUnusableConfiguredTool(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Tools.ResticPath = filepath.Join(f.base, "bin", "no-such-restic")
	_, err := f.build()
	if err == nil || !strings.Contains(err.Error(), "no-such-restic") {
		t.Fatalf("err = %v, want it to name the configured path that failed", err)
	}
}

func TestToolsUnavailableHintOmitsDownloadWhenAlreadyEnabled(t *testing.T) {
	t.Parallel()
	err := &toolsUnavailableError{
		err:      errors.New("rclone: boom"),
		manifest: tools.DefaultManifest(),
		toolsDir: "/t",
		download: true,
	}
	if strings.Contains(err.Error(), "tools.download") {
		t.Errorf("hint suggests enabling download that is already enabled:\n%s", err)
	}
	if !strings.Contains(err.Error(), "rclone: boom") {
		t.Errorf("underlying error missing:\n%s", err)
	}
}

func TestBuildWorkerSupervisorDeliversLinesOfRestic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Larger than the supervisor's 1 MiB default, as a restic snapshot list
	// of a big repository is.
	const size = 3 << 20
	var got, truncated int
	_, err = w.supervisor.Run(t.Context(), proc.Spec{
		Path: exe,
		Env:  map[string]string{helperLineEnv: strconv.Itoa(size)},
		Stdout: func(l proc.Line) {
			got = len(l.Data)
			if l.Truncated {
				truncated++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != size || truncated != 0 {
		t.Errorf("line of %d bytes delivered as %d bytes (truncated %d times)", size, got, truncated)
	}
	if maxToolLineBytes != 32<<20 {
		t.Errorf("maxToolLineBytes = %d, want 32 MiB", maxToolLineBytes)
	}
}
