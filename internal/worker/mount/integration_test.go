//go:build integration && (linux || windows)

package mount

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// Real mounts with the pinned rclone (SYNCR_TEST_RCLONE). They need FUSE 3
// on Linux (/dev/fuse, fusermount3, the right to mount) or WinFsp on
// Windows. Where the static checks fail they skip, unless
// SYNCR_REQUIRE_MOUNT=1 (CI), which makes them fail instead.

// requireMount returns a Mounter over the real rclone and supervisor, or
// skips (fails with SYNCR_REQUIRE_MOUNT=1) when this machine cannot mount.
func requireMount(t *testing.T) *Mounter {
	t.Helper()
	require := os.Getenv("SYNCR_REQUIRE_MOUNT") == "1"
	rclone := os.Getenv("SYNCR_TEST_RCLONE")
	if rclone == "" {
		if require {
			t.Fatal("SYNCR_REQUIRE_MOUNT=1 but SYNCR_TEST_RCLONE is not set")
		}
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	m, err := New(Config{Rclone: rclone, Supervisor: proc.New(proc.Options{}), Grace: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if reason := m.static(); reason != "" {
		if require {
			t.Fatalf("SYNCR_REQUIRE_MOUNT=1 but this machine cannot mount: %s", reason)
		}
		t.Skipf("this machine cannot mount: %s", reason)
	}
	return m
}

// mountFixture is a folder to mount and a folder of mount points.
type mountFixture struct {
	source, points, scratch string
}

func newMountFixture(t *testing.T) mountFixture {
	t.Helper()
	base := t.TempDir()
	f := mountFixture{source: filepath.Join(base, "remote"), points: filepath.Join(base, "sources", "mount"), scratch: filepath.Join(base, "scratch")}
	for name, data := range map[string]string{"a.txt": "alpha", filepath.Join("sub", "b.txt"): "beta", "c.tmp": "skip me"} {
		path := filepath.Join(f.source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.scratch, "filters.txt"), []byte("- *.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f mountFixture) request(key string) Request {
	return Request{
		Remote:  ":local:" + f.source,
		Point:   filepath.Join(f.points, key),
		DevName: "syncr-" + key,
		Flags:   []string{"--filter-from=" + filepath.Join(f.scratch, "filters.txt")},
		Env:     map[string]string{"RCLONE_CACHE_DIR": filepath.Join(f.scratch, "cache"), "RCLONE_ASK_PASSWORD": "false"},
		Dir:     f.scratch,
		Timeout: 30 * time.Second,
	}
}

const itKey = "0123456789abcdef0123456789abcdef"

// TestIntegrationMountReadOnly mounts a folder with a filter: the files
// read as they are, with their modification times, the filtered file is
// hidden, nothing can be written; unmounting leaves no mount. On Windows
// it also proves that rclone and WinFsp mount at a folder that does not
// exist under a parent that does.
func TestIntegrationMountReadOnly(t *testing.T) {
	m := requireMount(t)
	f := newMountFixture(t)
	req := f.request(itKey)
	mt, err := m.Mount(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mt.Unmount() }()

	data, err := os.ReadFile(filepath.Join(req.Point, "a.txt"))
	if err != nil || string(data) != "alpha" {
		t.Errorf("read a.txt = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(req.Point, "sub", "b.txt")); err != nil || string(data) != "beta" {
		t.Errorf("read sub/b.txt = %q, %v", data, err)
	}
	entries, err := os.ReadDir(req.Point)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"a.txt", "sub"}) {
		t.Errorf("mounted folder lists %q; the filter should hide c.tmp", names)
	}
	src, err := os.Stat(filepath.Join(f.source, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.Stat(filepath.Join(req.Point, "a.txt")); err != nil || !got.ModTime().Equal(src.ModTime()) {
		t.Errorf("modification time through the mount = %v, want %v (%v)", got.ModTime(), src.ModTime(), err)
	}
	if err := os.WriteFile(filepath.Join(req.Point, "new.txt"), []byte("x"), 0o644); err == nil {
		t.Error("wrote into a read-only mount")
	}
	if _, err := os.Stat(filepath.Join(f.source, "new.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the write reached the remote: %v", err)
	}

	if err := mt.Unmount(); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.plat.mounted(req.Point); ok || err != nil {
		t.Errorf("mounted after Unmount = %v, %v", ok, err)
	}
	if entries, err := os.ReadDir(req.Point); err == nil && len(entries) > 0 {
		t.Errorf("the mount point holds %v after Unmount", entries)
	}
}

// TestIntegrationMountEndsWithItsContext: canceling the job's context
// stops rclone, and Unmount leaves no mount.
func TestIntegrationMountEndsWithItsContext(t *testing.T) {
	m := requireMount(t)
	f := newMountFixture(t)
	req := f.request(itKey)
	ctx, cancel := context.WithCancel(t.Context())
	mt, err := m.Mount(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := mt.Unmount(); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.plat.mounted(req.Point); ok || err != nil {
		t.Errorf("mounted after a cancel = %v, %v", ok, err)
	}
}

// TestIntegrationSweepAfterKill kills an rclone mount the way a crash
// would (SIGKILL on Linux, TerminateProcess on Windows), then sweeps. On
// Linux the kill leaves a stale mount that the sweep unmounts lazily; on
// Windows WinFsp removes the mount when the process exits, and the sweep
// removes an empty leftover folder.
func TestIntegrationSweepAfterKill(t *testing.T) {
	m := requireMount(t)
	f := newMountFixture(t)
	req := f.request(itKey)
	if err := m.plat.prepare(req.Point); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), m.cfg.Rclone, Args(req)...)
	cmd.Dir = f.scratch
	cmd.Env = append(os.Environ(), "RCLONE_CACHE_DIR="+filepath.Join(f.scratch, "cache"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitMounted(t, m, req.Point, true)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	if runtime.GOOS == "windows" {
		// WinFsp unmounts when its process exits.
		waitMounted(t, m, req.Point, false)
	} else if ok, _ := m.plat.mounted(req.Point); !ok {
		t.Fatal("the killed mount left no stale mount to sweep")
	}
	swept, err := m.SweepAll(t.Context(), f.points)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("swept %q", swept)
	if runtime.GOOS != "windows" && len(swept) != 1 {
		t.Errorf("swept %q, want the killed mount", swept)
	}
	if ok, err := m.plat.mounted(req.Point); ok || err != nil {
		t.Errorf("mounted after the sweep = %v, %v", ok, err)
	}
	// The point is usable again.
	mt, err := m.Mount(t.Context(), req)
	if err != nil {
		t.Fatalf("mount after the sweep: %v", err)
	}
	if err := mt.Unmount(); err != nil {
		t.Fatal(err)
	}
}

// waitMounted polls until point is (or is not) mounted, for at most 30 s.
func waitMounted(t *testing.T, m *Mounter, point string, want bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, err := m.plat.mounted(point)
		if err == nil && ok == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mounted(%s) is not %v after 30s (%v)", point, want, err)
		}
		time.Sleep(100 * time.Millisecond) // the kernel or WinFsp is waited for, not this process
	}
}

// TestIntegrationTrialMount: on a machine that can mount, the trial mount
// at startup says so.
func TestIntegrationTrialMount(t *testing.T) {
	m := requireMount(t)
	base := t.TempDir()
	ok, reason := m.Detect(t.Context(), DetectOptions{Trial: true, MountsDir: filepath.Join(base, "mount"), ScratchDir: base})
	if !ok || reason != "" {
		t.Errorf("Detect = %v, %q", ok, reason)
	}
}
