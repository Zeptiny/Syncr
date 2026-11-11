//go:build integration

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

// versionVar is the variable release builds set with -ldflags -X.
const versionVar = "github.com/Zeptiny/Syncr/internal/platform/version.version"

// upgradeBin is a worker build of the upgrade test and the version it
// reports.
type upgradeBin struct{ path, version string }

// upgradeBinaries returns the three workers of the upgrade test: old (the
// installed one), new (a working release), and broken (a real worker built
// with -tags syncr_broken_release, whose "run" goes through the whole
// start, the start-up cleanup included, and then fails; brokenrelease.go).
// SYNCR_TEST_UPGRADE_BIN names a directory holding them as old/, new/ and
// broken/syncr-worker[.exe]; otherwise they are built here with the go
// command (SYNCR_TEST_GO, or go on PATH). The CI recipe builds the same
// three.
func upgradeBinaries(t *testing.T) (old, current, broken upgradeBin) {
	t.Helper()
	exe := "syncr-worker"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	dir := os.Getenv("SYNCR_TEST_UPGRADE_BIN")
	if dir == "" {
		dir = t.TempDir()
		goBin := os.Getenv("SYNCR_TEST_GO")
		if goBin == "" {
			var err error
			if goBin, err = exec.LookPath("go"); err != nil {
				t.Fatalf("the upgrade test builds workers: set SYNCR_TEST_GO or SYNCR_TEST_UPGRADE_BIN (%v)", err)
			}
		}
		for _, b := range []struct{ name, tags, ldflags string }{
			{"old", "", "-X " + versionVar + "=v2.0.0-test.1"},
			{"new", "", "-X " + versionVar + "=v2.0.0-test.2"},
			{"broken", "syncr_broken_release", "-X " + versionVar + "=v2.0.0-test.3"},
		} {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			cmd := exec.CommandContext(ctx, goBin, "build", "-tags", b.tags, "-ldflags", b.ldflags, "-o", filepath.Join(dir, b.name, exe), ".")
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("go build %s: %v\n%s", b.name, err, out)
			}
		}
	}
	bin := func(name string) upgradeBin {
		path, err := filepath.Abs(filepath.Join(dir, name, exe))
		if err != nil {
			t.Fatal(err)
		}
		return upgradeBin{path: path, version: binaryVersion(t, path)}
	}
	old, current, broken = bin("old"), bin("new"), bin("broken")
	if old.version == current.version || current.version == broken.version {
		t.Fatalf("the workers need different versions: %s, %s, %s", old.version, current.version, broken.version)
	}
	return old, current, broken
}

// binaryVersion runs "<path> version".
func binaryVersion(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), path, "version").Output()
	if f := strings.Fields(string(out)); err == nil && len(f) >= 2 && f[0] == "syncr-worker" {
		return f[1]
	}
	t.Fatalf("%s version: %q, %v", path, out, err)
	return ""
}

// serveReleases serves each binary of releases as a release under
// /<name>/: checksums.txt and syncr-worker_<os>_<arch>.tar.gz (.zip on
// Windows), with the binary at the archive's root, as the releases are
// built.
func serveReleases(t *testing.T, releases map[string]string) *httptest.Server {
	t.Helper()
	files := map[string][]byte{}
	for name, path := range releases {
		bin, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		asset := fmt.Sprintf("syncr-worker_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		if runtime.GOOS == "windows" {
			asset = fmt.Sprintf("syncr-worker_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
			zw := zip.NewWriter(&buf)
			w, err := zw.Create("syncr-worker.exe")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(bin)
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "syncr-worker", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write(bin)
			if err := errors.Join(tw.Close(), gz.Close()); err != nil {
				t.Fatal(err)
			}
		}
		sum := sha256.Sum256(buf.Bytes())
		files["/"+name+"/"+asset] = buf.Bytes()
		files["/"+name+"/checksums.txt"] = []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// copyManagedTool copies the managed install a pinned tool binary belongs
// to (<dir>/<tool>/<version>/<os>-<arch>/, with its install record) into
// toolsDir, so the upgrade's "tools fetch" finds it up to date instead of
// downloading it. A binary that is not a managed install is skipped.
func copyManagedTool(t *testing.T, tool, binary, toolsDir string) {
	t.Helper()
	platform := filepath.Dir(binary)
	version := filepath.Dir(platform)
	if filepath.Base(platform) != runtime.GOOS+"-"+runtime.GOARCH || filepath.Base(filepath.Dir(version)) != tool {
		t.Logf("%s is not a managed install; the upgrade downloads %s", binary, tool)
		return
	}
	dst := filepath.Join(toolsDir, tool, filepath.Base(version), filepath.Base(platform))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(platform)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(platform, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

// helloVersion asks the passive worker at addr for its link state, as the
// controller does (the pinned certificate, the pairing secret), and
// returns the version its Hello reports: the version of the process that
// serves it.
func helloVersion(ctx context.Context, addr, fingerprint, secret string) (string, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // the certificate is pinned below, as the controller pins it
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || tlscert.Fingerprint(raw[0]) != fingerprint {
				return errors.New("not the worker's certificate")
			}
			return nil
		},
	}}
	defer tr.CloseIdleConnections()
	cl := syncrv1connect.NewWorkerLinkServiceClient(&http.Client{Transport: tr, Timeout: 10 * time.Second}, "https://"+addr)
	req := connect.NewRequest(&syncrv1.PullRequest{})
	req.Header().Set("Authorization", link.PassiveAuthorization(secret))
	resp, err := cl.Pull(ctx, req)
	if err != nil {
		return "", err
	}
	for _, e := range resp.Msg.GetEvents() {
		if h := e.GetHello(); h != nil {
			return h.GetWorkerVersion(), nil
		}
	}
	return "", errors.New("no hello among the worker's events")
}

// TestIntegrationRealServiceUpgrade upgrades a worker installed as the real
// service of this operating system (a systemd unit, or a Windows Service)
// with "syncr-worker upgrade --from <a release server on this machine>":
//
//  1. the installed worker (old) is upgraded to a working release (new):
//     the command succeeds, and the service runs new;
//  2. then to a release that fails after its whole start (broken): the
//     command fails, the old binary is put back from the backup the
//     upgrade kept (the broken worker's start-up cleanup must not have
//     removed it), and the service runs new again.
//
// The service's version is the one its own Hello reports over the passive
// link. It changes the machine, so it runs only with SYNCR_TEST_SERVICE=1,
// as root or from an elevated terminal (CI runs it on throw-away runners);
// SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC are the pinned tools. The service
// is always removed again.
func TestIntegrationRealServiceUpgrade(t *testing.T) {
	if os.Getenv("SYNCR_TEST_SERVICE") != "1" {
		t.Skip("set SYNCR_TEST_SERVICE=1 to install and upgrade a real service (needs root or Administrator)")
	}
	if !service.Elevated() {
		t.Fatal("SYNCR_TEST_SERVICE=1 needs root (Linux) or an elevated terminal (Windows)")
	}
	rclone, restic := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rclone == "" || restic == "" {
		t.Fatal("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC must name the pinned tools")
	}
	old, current, broken := upgradeBinaries(t)
	releases := serveReleases(t, map[string]string{"new": current.path, "broken": broken.path})

	// The installed worker: the old binary in a directory of its own, with
	// the pinned tools already in its tools directory.
	base := t.TempDir()
	exe := filepath.Join(base, "bin", filepath.Base(old.path))
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(old.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, data := filepath.Join(base, "worker.yaml"), filepath.Join(base, "data")
	copyManagedTool(t, "rclone", rclone, filepath.Join(data, "tools"))
	copyManagedTool(t, "restic", restic, filepath.Join(data, "tools"))

	// Never touch a worker service that is really in use.
	mgr := service.NewManager()
	spec := service.NewSpec(runtime.GOOS, service.ScopeSystem, exe, cfg, data, "")
	if st, err := mgr.Status(t.Context(), spec); err != nil || st != service.StateNotInstalled {
		t.Fatalf("a %s service already exists on this machine (state %v, err %v); refusing to replace it", spec.Name, st, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = mgr.Uninstall(ctx, spec)
		if st, _ := mgr.Status(ctx, spec); st != service.StateNotInstalled {
			t.Errorf("cleanup: the service is still %v", st)
		}
	})

	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Env = append(os.Environ(), "SYNCR_RCLONE_PATH="+rclone, "SYNCR_RESTIC_PATH="+restic)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		t.Logf("$ syncr-worker %s\n%s", strings.Join(args, " "), out.String())
		return out.String(), err
	}
	port := freePort(t)
	addr := "127.0.0.1:" + port
	out, err := run("setup", "--passive", "--listen", addr, "--advertise-url", "https://"+addr, "--data-dir", data, "--config", cfg)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	fingerprint := field(t, out, `Fingerprint:\s+(\S+)`)
	secret := field(t, out, `Pairing secret:\s+(\S+)`)

	// serving waits until the service runs and its process reports want.
	serving := func(want string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		var got string
		var err error
		for {
			st, serr := mgr.Status(t.Context(), spec)
			if serr == nil && st == service.StateRunning {
				if got, err = helloVersion(t.Context(), addr, fingerprint, secret); err == nil && got == want {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the service does not run %s: state %v (%v), its hello says %q (%v)", want, st, serr, got, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	serving(old.version)

	// 1. A working release: the service runs it.
	out, err = run("upgrade", "--from", releases.URL+"/new", "--config", cfg)
	if err != nil {
		t.Fatalf("upgrade to %s: %v", current.version, err)
	}
	for _, want := range []string{"Verified syncr-worker_", "Upgraded from " + old.version + " to " + current.version} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade output lacks %q", want)
		}
	}
	serving(current.version)
	if got := binaryVersion(t, exe); got != current.version {
		t.Errorf("the installed binary reports %s, want %s", got, current.version)
	}

	// 2. A release that does not stay up: rolled back, the command fails,
	// and the service runs the version it ran before.
	out, err = run("upgrade", "--from", releases.URL+"/broken", "--config", cfg)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitError {
		t.Fatalf("upgrade to the broken release = %v, want exit status %d", err, exitError)
	}
	for _, want := range []string{"the upgrade was rolled back", current.version + " was put back and started"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade output lacks %q", want)
		}
	}
	// Put back from the backup itself: the broken worker started, ran its
	// start-up cleanup and the backup survived it.
	if strings.Contains(out, "is missing") {
		t.Errorf("the backup was gone before the rollback (removed by the new worker's start?)")
	}
	serving(current.version)
	// Nothing is left next to the binary but the upgrade's lock (on Windows
	// the old binary of the first upgrade may still wait for a start).
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch name := e.Name(); {
		case name == filepath.Base(exe), name == ".syncr-worker-upgrade.lock":
		case runtime.GOOS == "windows" && name == filepath.Base(exe)+".old":
		default:
			t.Errorf("left next to the binary: %s", name)
		}
	}
	if got := binaryVersion(t, exe); got != current.version {
		t.Errorf("after the rollback the installed binary reports %s, want %s", got, current.version)
	}
}

// field returns the first group of pattern in out.
func field(t *testing.T, out, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("setup output lacks %s", pattern)
	}
	return m[1]
}

// TestIntegrationUpgradeNoRestart runs "syncr-worker upgrade --no-restart"
// with real builds and needs no rights: a worker set up without a service
// is upgraded from a release server on this machine. It covers what the
// service test does up to the swap (download, checksum, extraction, the
// new binary's version and tools commands with the pinned tools, the
// swap itself, on Windows the rename), plus a dry run that changes
// nothing. SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC are the pinned tools;
// the workers are built as for TestIntegrationRealServiceUpgrade.
func TestIntegrationUpgradeNoRestart(t *testing.T) {
	rclone, restic := os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rclone == "" || restic == "" {
		t.Skip("SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC not set")
	}
	old, current, _ := upgradeBinaries(t)
	releases := serveReleases(t, map[string]string{"new": current.path})

	base := t.TempDir()
	exe := filepath.Join(base, "bin", filepath.Base(old.path))
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(old.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	data, cfg := filepath.Join(base, "data"), filepath.Join(base, "worker.yaml")
	toolsDir := filepath.Join(data, "tools")
	copyManagedTool(t, "rclone", rclone, toolsDir)
	copyManagedTool(t, "restic", restic, toolsDir)
	conf := fmt.Sprintf("data_dir: %q\ntools:\n  rclone_path: %q\n  restic_path: %q\n", data, rclone, restic)
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	upgrade := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, append([]string{"upgrade", "--from", releases.URL + "/new", "--config", cfg}, args...)...)
		out, err := cmd.CombinedOutput()
		t.Logf("$ syncr-worker upgrade %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("upgrade %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}

	if out := upgrade("--dry-run", "--no-restart"); !strings.Contains(out, "Nothing was changed.") || binaryVersion(t, exe) != old.version {
		t.Fatalf("the dry run changed the worker (now %s)", binaryVersion(t, exe))
	}
	out := upgrade("--no-restart", "--version", current.version)
	if got := binaryVersion(t, exe); got != current.version {
		t.Fatalf("after the upgrade the binary reports %s, want %s", got, current.version)
	}
	// The upgrade names the binary by its resolved path (on Windows the
	// long form of a short 8.3 temporary path such as RUNNER~1).
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Verified syncr-worker_", "Replaced " + resolved, "Restart the worker to run " + current.version} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade output lacks %q", want)
		}
	}
	// The new binary is the one served, bit for bit.
	if got, err := os.ReadFile(exe); err != nil || !bytes.Equal(got, mustRead(t, current.path)) {
		t.Errorf("the installed binary differs from the release (%v)", err)
	}
	// Already the requested version: nothing to do.
	if out := upgrade("--no-restart", "--version", current.version); !strings.Contains(out, "is already installed; nothing to do") {
		t.Errorf("a second upgrade did something")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
