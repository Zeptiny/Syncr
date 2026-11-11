package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/datadir"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// TestRunRefusesASecondWorkerOnItsDataDir: `run` takes the data
// directory's lock before it builds anything, so a second worker on the
// same directory exits with an explanation, and the first one's files are
// left alone.
func TestRunRefusesASecondWorkerOnItsDataDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	release, err := datadir.Lock(f.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	leftover := filepath.Join(paths.ScratchDir(f.cfg.DataDir), "rclone", "rclone-job-1", "rclone.conf")
	writeFile(t, leftover)

	e, stderr := f.runEnv(nil)
	if code := run(t.Context(), []string{"run"}, e); code != exitError {
		t.Fatalf("exit code %d, want %d", code, exitError)
	}
	for _, want := range []string{"syncr-worker run:", "another worker is already running with this data directory", f.cfg.DataDir, "SYNCR_DATA_DIR"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr.String(), `"msg":"worker ready"`) {
		t.Error("the second worker got ready")
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("the refused worker touched the scratch directory of the running one: %v", err)
	}
}

// TestRunCleansItsScratchAndHoldsTheLock: a starting worker removes what
// jobs of an earlier process left in its scratch directories (and only
// there), holds the data directory's lock while it runs and releases it
// when it stops.
func TestRunCleansItsScratchAndHoldsTheLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scratch := paths.ScratchDir(f.cfg.DataDir)
	leftovers := []string{
		filepath.Join(scratch, "rclone", "rclone-job-1", "rclone.conf"),
		filepath.Join(scratch, "rclone", "probe-7", "cache", "x"),
		filepath.Join(scratch, "restic", "restic-job-2", "tmp", "pack"),
	}
	kept := filepath.Join(f.cfg.DataDir, "cache", "restic", "keep")
	for _, p := range append(leftovers, kept) {
		writeFile(t, p)
	}

	ctx, cancel := context.WithCancel(t.Context())
	e, stderr := f.runEnv(nil)
	ready := notifyReady(e)
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run"}, e) }()
	awaitReady(t, ready, done, stderr)

	for _, p := range leftovers {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the start: %v", p, err)
		}
	}
	for _, dir := range []string{filepath.Join(scratch, "rclone"), filepath.Join(scratch, "restic"), kept} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s is gone: %v", dir, err)
		}
	}
	if _, err := datadir.Lock(f.cfg.DataDir); err == nil {
		t.Error("the data directory is not locked while the worker runs")
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit code %d: %s", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	release, err := datadir.Lock(f.cfg.DataDir)
	if err != nil {
		t.Fatalf("the lock is still held after the worker stopped: %v", err)
	}
	_ = release()
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSetupProvisioningPrunesOldToolVersions: once setup has the pinned
// tools, it removes the managed installs of other versions from the tools
// directory and says so; the pinned versions and anything that is not a
// version directory stay. A tools directory that does not exist is left
// alone.
func TestSetupProvisioningPrunesOldToolVersions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e, _ := f.runEnv(nil)
	p := toolProvisioner{e: e}
	toolsDir := filepath.Join(f.base, "tools")
	old := filepath.Join(toolsDir, toolRclone, "0.0.1", "linux-amd64", "rclone")
	var keep []string
	for _, pin := range tools.DefaultManifest().Pins() {
		keep = append(keep, filepath.Join(toolsDir, pin.Tool, pin.Version, "linux-amd64", pin.Tool))
	}
	keep = append(keep, filepath.Join(toolsDir, toolRclone, "README"))
	for _, path := range append([]string{old}, keep...) {
		writeFile(t, path)
	}

	var progress []string
	got, err := p.Provision(t.Context(), toolsDir, func(msg string) { progress = append(progress, msg) })
	if err != nil || len(got) != 2 {
		t.Fatalf("Provision = %v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(old))); !os.IsNotExist(err) {
		t.Errorf("the old rclone version survived: %v", err)
	}
	for _, path := range keep {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
	if !slices.ContainsFunc(progress, func(m string) bool { return strings.Contains(m, filepath.Join(toolsDir, toolRclone, "0.0.1")) }) {
		t.Errorf("progress does not report the removal: %q", progress)
	}

	missing := filepath.Join(f.base, "no-tools")
	progress = nil
	if _, err := p.Provision(t.Context(), missing, func(msg string) { progress = append(progress, msg) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) || len(progress) != 1 {
		t.Errorf("a missing tools directory: stat %v, progress %q", err, progress)
	}
}
