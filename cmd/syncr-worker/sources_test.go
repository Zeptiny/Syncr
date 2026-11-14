package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/mount"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

// fakeDetect answers the mount detection and records its options.
type fakeDetect struct {
	ok     bool
	reason string
	opts   []mount.DetectOptions
}

func (d *fakeDetect) detect(_ context.Context, _ *mount.Mounter, opts mount.DetectOptions) (bool, string) {
	d.opts = append(d.opts, opts)
	return d.ok, d.reason
}

// TestBuildWorkerReportsRemoteSources: the Hello's remote_sources follow
// the configuration and the detection, with the reasons.
func TestBuildWorkerReportsRemoteSources(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		cfg     func(*workerconfig.RemoteSourcesConfig)
		detect  fakeDetect
		mirror  bool
		mount   bool
		reasons [2]string
		off     bool
		trial   bool
	}{
		"defaults, mountable": {cfg: func(*workerconfig.RemoteSourcesConfig) {}, detect: fakeDetect{ok: true}, mirror: true, mount: true, trial: true},
		"no FUSE": {cfg: func(*workerconfig.RemoteSourcesConfig) {}, detect: fakeDetect{reason: "/dev/fuse is missing"}, mirror: true,
			reasons: [2]string{"", "/dev/fuse is missing"}, trial: true},
		"everything off": {cfg: func(c *workerconfig.RemoteSourcesConfig) { c.Mirror, c.Mount = false, workerconfig.MountOff },
			detect: fakeDetect{reason: mount.ReasonOff}, reasons: [2]string{mirrorOffReason, mount.ReasonOff}, off: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tt.cfg(&f.cfg.RemoteSources)
			d := tt.detect
			f.deps.detectMount = d.detect
			w, err := f.build()
			if err != nil {
				t.Fatal(err)
			}
			rs := w.remoteSources
			if rs.GetMirror() != tt.mirror || rs.GetMount() != tt.mount || rs.GetMirrorReason() != tt.reasons[0] || rs.GetMountReason() != tt.reasons[1] {
				t.Errorf("remote sources = %v", rs)
			}
			if id := identity(w, linkInfo{hostname: func() (string, error) { return "h", nil }}); id.RemoteSources != rs {
				t.Errorf("the Hello's remote sources = %v", id.RemoteSources)
			}
			if len(d.opts) != 1 || d.opts[0].Off != tt.off || d.opts[0].Trial != tt.trial ||
				d.opts[0].MountsDir != paths.MountsDir(f.cfg.DataDir) || d.opts[0].ScratchDir != filepath.Join(paths.ScratchDir(f.cfg.DataDir), "mount") {
				t.Errorf("detect options = %+v", d.opts)
			}
		})
	}
}

// TestBuildWorkerOnThisMachine: the real detection runs; without /dev/fuse
// the reason says so.
func TestBuildWorkerOnThisMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("remote sources on this machine: %v", w.remoteSources)
	if !w.remoteSources.GetMirror() || (!w.remoteSources.GetMount() && w.remoteSources.GetMountReason() == "") {
		t.Errorf("remote sources = %v", w.remoteSources)
	}
	if _, err := os.Stat("/dev/fuse"); errors.Is(err, os.ErrNotExist) && w.remoteSources.GetMountReason() != "/dev/fuse is missing" &&
		!strings.Contains(w.remoteSources.GetMountReason(), "not supported") && !strings.Contains(w.remoteSources.GetMountReason(), "WinFsp") {
		t.Errorf("mount reason = %q", w.remoteSources.GetMountReason())
	}
}

// TestBuildWorkerReservesTheSourcesFolder: even with the data directory
// inside local_roots, no job, probe or listing may use <data_dir>/sources.
func TestBuildWorkerReservesTheSourcesFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.LocalRoots = []string{f.base}
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	sources := paths.SourcesDir(f.cfg.DataDir)
	for _, p := range []string{sources, filepath.Join(sources, "mirror", strings.Repeat("a", 32), "data")} {
		if _, err := w.policy.Resolve(p); !errors.Is(err, localfs.ErrReserved) {
			t.Errorf("Resolve(%q) = %v", p, err)
		}
	}
	if _, err := w.policy.Resolve(paths.BisyncDir(f.cfg.DataDir)); err != nil {
		t.Errorf("the rest of the data directory: %v", err)
	}
	if w.sources.Root() != sources {
		t.Errorf("sources root = %q", w.sources.Root())
	}
	// The marker that keeps backups of a folder holding it out is there
	// from the start.
	if _, err := os.Stat(filepath.Join(sources, sourcestate.MarkerName)); err != nil {
		t.Errorf("marker: %v", err)
	}
}

// TestStartBackgroundPrunesMirrors removes mirrors unused for
// remote_sources.mirror_retention and keeps the others.
func TestStartBackgroundPrunesMirrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.RemoteSources.MirrorRetention = 48 * time.Hour
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	mirrors := paths.MirrorsDir(f.cfg.DataDir)
	stale := filepath.Join(mirrors, strings.Repeat("a", 32))
	recent := filepath.Join(mirrors, strings.Repeat("b", 32))
	for dir, at := range map[string]time.Time{stale: time.Now().Add(-72 * time.Hour), recent: time.Now().Add(-24 * time.Hour)} {
		if err := os.MkdirAll(filepath.Join(dir, sourcestate.DataDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sourcestate.LastRunFileName), []byte(at.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stop := w.startBackground(t.Context(), f.log)
	stop()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale mirror kept: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent mirror removed: %v", err)
	}
	if !strings.Contains(f.logs.String(), "component=sources") {
		t.Errorf("logs lack the removal:\n%s", f.logs)
	}
}
