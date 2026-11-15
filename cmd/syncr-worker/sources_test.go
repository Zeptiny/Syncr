package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// TestBuildWorkerOnThisMachine: the real detection runs. The fixture's
// rclone is not a real program, so the worker never reports that it can
// mount, and it says why: on Linux without /dev/fuse the static check says
// so before any trial; on Windows the static check names WinFsp when it is
// missing, and with WinFsp installed (as on CI) the trial mount runs the
// fixture's rclone and fails; elsewhere mounting is not supported.
func TestBuildWorkerOnThisMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("remote sources on this machine: %v", w.remoteSources)
	reason := w.remoteSources.GetMountReason()
	if !w.remoteSources.GetMirror() || w.remoteSources.GetMount() || reason == "" {
		t.Errorf("remote sources = %v", w.remoteSources)
	}
	switch runtime.GOOS {
	case "linux":
		if _, err := os.Stat("/dev/fuse"); errors.Is(err, os.ErrNotExist) && reason != "/dev/fuse is missing" {
			t.Errorf("mount reason without /dev/fuse = %q", reason)
		}
	case "windows":
		if !strings.Contains(reason, "WinFsp") && !strings.HasPrefix(reason, "a trial mount failed") {
			t.Errorf("mount reason = %q, want WinFsp missing or a failed trial with the fixture's rclone", reason)
		}
	default:
		if !strings.Contains(reason, "not supported") {
			t.Errorf("mount reason = %q", reason)
		}
	}
}

// TestBuildWorkerReservesItsOwnFolders: even with the data directory
// inside local_roots, no job, probe or listing may use the data directory
// (<data_dir>/sources, bisync, scratch...), the tools directory, the config
// file or the TLS files; the marker files that keep backups of a folder
// holding them out are there from the start.
func TestBuildWorkerReservesItsOwnFolders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.LocalRoots = []string{f.base}
	configDir := filepath.Join(f.base, "etc")
	f.deps.configFile = filepath.Join(configDir, "worker.yaml")
	f.cfg.Link.TLSKeyFile = filepath.Join(configDir, "tls", "key.pem")
	w, err := f.build()
	if err != nil {
		t.Fatal(err)
	}
	sources := paths.SourcesDir(f.cfg.DataDir)
	for _, p := range []string{
		sources, filepath.Join(sources, "mirror", strings.Repeat("a", 32), "data"),
		f.cfg.DataDir, paths.BisyncDir(f.cfg.DataDir), paths.ScratchDir(f.cfg.DataDir),
		f.deps.configFile, f.cfg.Link.TLSKeyFile,
	} {
		if _, err := w.policy.Resolve(p); !errors.Is(err, localfs.ErrReserved) {
			t.Errorf("Resolve(%q) = %v", p, err)
		}
	}
	for _, p := range []string{f.base, configDir, filepath.Join(configDir, "other.yaml")} {
		if _, err := w.policy.Resolve(p); err != nil {
			t.Errorf("Resolve(%q) = %v", p, err)
		}
	}
	if w.sources.Root() != sources {
		t.Errorf("sources root = %q", w.sources.Root())
	}
	for _, marker := range []string{filepath.Join(sources, sourcestate.MarkerName), filepath.Join(f.cfg.DataDir, paths.DataMarkerName)} {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("marker: %v", err)
		}
	}
}

// TestReservedPaths: the data directory, and the tools directory, config
// and TLS files outside it, made absolute.
func TestReservedPaths(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	var cfg workerconfig.Config
	cfg.Link.TLSCertFile = filepath.Join(data, "tls", "cert.pem")
	cfg.Link.TLSKeyFile = filepath.Join(base, "tls", "key.pem")
	got := reservedPaths(cfg, data, filepath.Join(data, "tools"), filepath.Join(base, "etc", "worker.yaml"))
	want := []string{data, filepath.Join(base, "etc", "worker.yaml"), filepath.Join(base, "tls", "key.pem")}
	if !slices.Equal(got, want) {
		t.Errorf("reservedPaths() = %q, want %q", got, want)
	}
	got = reservedPaths(workerconfig.Config{}, data, filepath.Join(base, "tools"), "")
	if want := []string{data, filepath.Join(base, "tools")}; !slices.Equal(got, want) {
		t.Errorf("reservedPaths() = %q, want %q", got, want)
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
