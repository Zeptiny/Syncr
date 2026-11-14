//go:build integration && (linux || windows)

package restic

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/mount"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Backups of a folder of a remote through a real rclone mount. They need
// FUSE 3 (Linux) or WinFsp (Windows) and skip where the machine cannot
// mount, unless SYNCR_REQUIRE_MOUNT=1 (CI), which makes them fail instead.

// itMounter adapts *mount.Mounter to the runner's port, as the worker's
// wiring does.
type itMounter struct{ m *mount.Mounter }

func (a itMounter) Mount(ctx context.Context, req mount.Request) (Mounted, error) {
	mt, err := a.m.Mount(ctx, req)
	if err != nil {
		return nil, err
	}
	return mt, nil
}

// requireMounter returns a real Mounter, or skips (fails with
// SYNCR_REQUIRE_MOUNT=1) when this machine cannot mount.
func requireMounter(t *testing.T) *mount.Mounter {
	t.Helper()
	m, err := mount.New(mount.Config{Rclone: toolPath(t, "SYNCR_TEST_RCLONE"), Supervisor: proc.New(proc.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := m.Detect(t.Context(), mount.DetectOptions{}); !ok {
		if os.Getenv("SYNCR_REQUIRE_MOUNT") == "1" {
			t.Fatalf("SYNCR_REQUIRE_MOUNT=1 but this machine cannot mount: %s", reason)
		}
		t.Skipf("this machine cannot mount: %s", reason)
	}
	return m
}

// TestIntegrationRemoteSourceMount backs up a local folder through a
// mount twice: the second run finds every file unmodified (rclone keeps
// the modification times and --ignore-inode covers the inode numbers);
// the mount is gone after each run, and after a run cancelled mid-backup.
func TestIntegrationRemoteSourceMount(t *testing.T) {
	m := requireMounter(t)
	slow := &atomic.Bool{}
	h := newHarnessWith(t, func(_ *harness, cfg *Config) {
		cfg.Supervisor = slowRestic{Supervisor: cfg.Supervisor, restic: cfg.ResticPath, on: slow}
	})
	remote := localSource(h)
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true, Mount: true}, itMounter{m: m})
	h.init(itPassword)
	writeFile(t, filepath.Join(remote, "photos", "a.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(remote, "photos", "2024", "b.jpg"), randomBytes(t, 2000))
	writeFile(t, filepath.Join(remote, "photos", "c.tmp"), []byte("x"))
	viaMount := func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
		s.Method = mountMethod
		s.Filters = []*syncrv1.FilterRule{excludeRule("*.tmp")}
	}
	point := filepath.Join(store.MountsDir(), sourceKey)
	unmounted := func(when string) {
		t.Helper()
		if entries, err := os.ReadDir(point); err == nil && len(entries) > 0 {
			t.Errorf("%s: the mount point still shows %d entries", when, len(entries))
		}
	}

	out := h.mustRemoteRun("src", "photos", viaMount)
	if out.GetFilesNew() != 2 || out.GetRemoteSource().GetMethod() != mountMethod {
		t.Errorf("first run = %v", out)
	}
	if got := h.tree(out.GetSnapshotId(), "/"); len(got) != 2 || got[0] != "2024/" || got[1] != "a.jpg" {
		t.Errorf("snapshot tree = %q", got)
	}
	unmounted("after the first run")
	out = h.mustRemoteRun("src", "photos", viaMount)
	if out.GetFilesUnmodified() != 2 || out.GetFilesChanged() != 0 {
		t.Errorf("second run = %v", out)
	}
	unmounted("after the second run")

	// Cancel while restic reads the mount (restic slowed down).
	writeFile(t, filepath.Join(remote, "photos", "big.bin"), randomBytes(t, 8<<20))
	slow.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := h.remoteRun(ctx, "src", "photos", func(p runner.Progress) {
		if p.Phase == "backup" && p.Bytes > 0 {
			cancel()
		}
	}, viaMount)
	if !runner.Canceled(err) {
		t.Fatalf("cancelled run = %v", err)
	}
	unmounted("after a cancelled run")
}

// slowRestic adds --limit-upload to restic's runs while on is set; rclone
// runs as it is.
type slowRestic struct {
	Supervisor
	restic string
	on     *atomic.Bool
}

func (s slowRestic) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	if s.on.Load() && spec.Path == s.restic {
		spec.Args = append([]string{"--limit-upload=256"}, spec.Args...)
	}
	return s.Supervisor.Run(ctx, spec)
}
