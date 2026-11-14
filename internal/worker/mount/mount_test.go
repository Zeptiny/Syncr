package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// fakeStarter plays rclone mount: run decides what the process does with
// its spec and context. It records every spec.
type fakeStarter struct {
	mu    sync.Mutex
	specs []proc.Spec
	run   func(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

func (f *fakeStarter) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	return f.run(ctx, spec)
}

func (f *fakeStarter) started() []proc.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// untilStopped is an rclone mount that serves until it is stopped, as the
// supervisor reports it.
func untilStopped(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	<-ctx.Done()
	return proc.Result{ExitCode: 0}, &proc.CanceledError{Path: spec.Path, Cause: context.Cause(ctx)}
}

// exits is an rclone mount that prints lines on standard error and exits
// with code.
func exits(code int, lines ...string) func(context.Context, proc.Spec) (proc.Result, error) {
	return func(_ context.Context, spec proc.Spec) (proc.Result, error) {
		for _, l := range lines {
			spec.Stderr(proc.Line{Data: []byte(l)})
		}
		return proc.Result{ExitCode: code}, &proc.ExitError{Path: spec.Path, ExitCode: code}
	}
}

// fakeClock hands out channels the test fills: ticks for the readiness
// polls, expired for the timeout.
type fakeClock struct {
	ticks   chan time.Time
	expired chan time.Time
}

func newFakeClock(ticks int, expired bool) *fakeClock {
	c := &fakeClock{ticks: make(chan time.Time, ticks), expired: make(chan time.Time, 1)}
	for range ticks {
		c.ticks <- time.Time{}
	}
	if expired {
		c.expired <- time.Time{}
	}
	return c
}

func (c *fakeClock) Ticker(time.Duration) (<-chan time.Time, func()) { return c.ticks, func() {} }
func (c *fakeClock) After(time.Duration) (<-chan time.Time, func()) {
	return c.expired, func() {}
}

// fakePlatform records what the Mounter asked of the operating system;
// mounted answers from readyAfter (the poll from which the folder is
// mounted; 0: never).
type fakePlatform struct {
	mu         sync.Mutex
	calls      []string
	polls      int
	readyAfter int
	sweepErr   error
	static     string
}

func (p *fakePlatform) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *fakePlatform) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *fakePlatform) platform() platform {
	return platform{
		static: func() string { p.record("static"); return p.static },
		sweep: func(_ context.Context, point string) error {
			p.record("sweep " + filepath.Base(point))
			return p.sweepErr
		},
		prepare: func(point string) error { p.record("prepare " + filepath.Base(point)); return nil },
		mounted: func(string) (bool, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.polls++
			return p.readyAfter > 0 && p.polls >= p.readyAfter, nil
		},
		cleanup: func(_ context.Context, point string) error {
			p.record("cleanup " + filepath.Base(point))
			return nil
		},
		sweepAll: func(context.Context, string) ([]string, error) { p.record("sweepAll"); return nil, nil },
	}
}

func newTestMounter(t *testing.T, run func(context.Context, proc.Spec) (proc.Result, error), plat *fakePlatform, clock Clock) (*Mounter, *fakeStarter) {
	t.Helper()
	st := &fakeStarter{run: run}
	m, err := New(Config{Rclone: "/fake/rclone", Supervisor: st, Clock: clock, Grace: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m.plat = plat.platform()
	return m, st
}

func testRequest(point string) Request {
	return Request{
		Remote:  "r2:bucket/photos",
		Point:   point,
		DevName: "syncr-0123",
		Flags:   []string{"--filter-from=/scratch/filters.txt", "--bwlimit=1000B"},
		Env:     map[string]string{"RCLONE_CONFIG_R2_TYPE": "s3"},
		Dir:     "/scratch",
		Timeout: 30 * time.Second,
	}
}

func TestArgs(t *testing.T) {
	t.Parallel()
	got := Args(testRequest("/data/sources/mount/0123"))
	want := []string{
		"mount", "--config=", "--read-only", "--vfs-cache-mode=off", "--dir-cache-time=1000h", "--poll-interval=0", "--attr-timeout=1h",
		"--use-json-log", "--log-level=NOTICE", "--devname=syncr-0123", "--filter-from=/scratch/filters.txt", "--bwlimit=1000B",
		"--", "r2:bucket/photos", "/data/sources/mount/0123",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args =\n%q\nwant\n%q", got, want)
	}
}

// TestMountLifecycle: sweep, prepare, start rclone, poll until mounted;
// Unmount stops rclone, waits for it and cleans up, once.
func TestMountLifecycle(t *testing.T) {
	t.Parallel()
	plat := &fakePlatform{readyAfter: 2}
	m, st := newTestMounter(t, untilStopped, plat, newFakeClock(2, false))
	point := filepath.Join(t.TempDir(), "0123")
	mt, err := m.Mount(t.Context(), testRequest(point))
	if err != nil {
		t.Fatal(err)
	}
	if mt.Point() != point || plat.polls != 2 {
		t.Errorf("point %q after %d polls", mt.Point(), plat.polls)
	}
	if got := plat.recorded(); !slices.Equal(got, []string{"sweep 0123", "prepare 0123"}) {
		t.Errorf("calls before Unmount = %q", got)
	}
	if err := mt.Unmount(); err != nil {
		t.Fatal(err)
	}
	specs := st.started()
	if len(specs) != 1 || specs[0].Path != "/fake/rclone" || specs[0].Dir != "/scratch" || specs[0].GracePeriod != 3*time.Second ||
		specs[0].Env["RCLONE_CONFIG_R2_TYPE"] != "s3" || !slices.Equal(specs[0].Args, Args(testRequest(point))) {
		t.Errorf("started %+v", specs)
	}
	if err := mt.Unmount(); err != nil {
		t.Errorf("second Unmount = %v", err)
	}
	if got := plat.recorded(); !slices.Equal(got, []string{"sweep 0123", "prepare 0123", "cleanup 0123"}) {
		t.Errorf("calls = %q", got)
	}
}

// TestMountRcloneExitsFirst: rclone ending before the folder is mounted
// fails the mount at once with its messages, Unavailable when they name
// FUSE or WinFsp (recorded from rclone v1.75.2 on a machine without
// /dev/fuse), and the mount point is cleaned up.
func TestMountRcloneExitsFirst(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-key"
	for name, tt := range map[string]struct {
		lines       []string
		unavailable bool
		want        string
	}{
		"no FUSE device": {
			lines: []string{
				`{"time":"2026-11-14T01:57:07.61422137Z","level":"notice","msg":"mount helper error: fusermount3: fuse device not found, try 'modprobe fuse' first","source":"fuse@v0.0.0-20230120002735-62a210ff1fd5/mount.go:23"}`,
				`{"time":"2026-11-14T01:57:07.614326107Z","level":"critical","msg":"Fatal error: failed to mount FUSE fs: fusermount: exit status 1","source":"mountlib/mount.go:329"}`,
			},
			unavailable: true,
			want: "rclone mount failed (exit code 1): mount helper error: fusermount3: fuse device not found, try 'modprobe fuse' first; " +
				"Fatal error: failed to mount FUSE fs: fusermount: exit status 1",
		},
		"no WinFsp": {
			lines:       []string{`{"level":"critical","msg":"Fatal error: failed to mount FUSE fs: cannot find winfsp"}`},
			unavailable: true,
			want:        "rclone mount failed (exit code 1): Fatal error: failed to mount FUSE fs: cannot find winfsp",
		},
		"mount point not empty": {
			lines:       []string{`{"level":"critical","msg":"Fatal error: failed to mount FUSE fs: \"/data/sources/mount/0123\" is not empty, use --allow-non-empty to mount anyway"}`},
			unavailable: false,
			want:        "is not empty",
		},
		"remote error with a secret": {
			lines:       []string{`{"level":"critical","msg":"Fatal error: bad key ` + secret + `"}`},
			unavailable: false,
			want:        "Fatal error: bad key [redacted]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plat := &fakePlatform{readyAfter: 1}
			m, _ := newTestMounter(t, exits(1, tt.lines...), plat, newFakeClock(0, false))
			req := testRequest(filepath.Join(t.TempDir(), "0123"))
			req.Scrub = scrub.New([]string{secret})
			mt, err := m.Mount(t.Context(), req)
			me, ok := errors.AsType[*Error](err)
			if mt != nil || !ok || me.Unavailable != tt.unavailable || !strings.Contains(me.Msg, tt.want) || strings.Contains(me.Msg, secret) {
				t.Fatalf("Mount = %v, %#v", mt, err)
			}
			if got := plat.recorded(); !slices.Contains(got, "cleanup 0123") {
				t.Errorf("calls = %q", got)
			}
		})
	}
}

func TestMountTimesOut(t *testing.T) {
	t.Parallel()
	plat := &fakePlatform{}
	var stopped bool
	m, _ := newTestMounter(t, func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		res, err := untilStopped(ctx, spec)
		stopped = true
		return res, err
	}, plat, newFakeClock(0, true))
	mt, err := m.Mount(t.Context(), testRequest(filepath.Join(t.TempDir(), "0123")))
	me, ok := errors.AsType[*Error](err)
	if mt != nil || !ok || me.Unavailable || me.Msg != "the remote was not mounted within 30s (remote_sources.mount_timeout)" {
		t.Fatalf("Mount = %v, %v", mt, err)
	}
	if !stopped || !slices.Contains(plat.recorded(), "cleanup 0123") {
		t.Errorf("rclone stopped = %v, calls = %q", stopped, plat.recorded())
	}
}

func TestMountCanceled(t *testing.T) {
	t.Parallel()
	plat := &fakePlatform{}
	m, _ := newTestMounter(t, untilStopped, plat, newFakeClock(0, false))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	mt, err := m.Mount(ctx, testRequest(filepath.Join(t.TempDir(), "0123")))
	if mt != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Mount = %v, %v", mt, err)
	}
	if !slices.Contains(plat.recorded(), "cleanup 0123") {
		t.Errorf("calls = %q", plat.recorded())
	}
}

// TestMountRefusals: nothing starts when mounting is not supported, the
// point is relative or the sweep fails; a binary that cannot start makes
// the machine unable to mount.
func TestMountRefusals(t *testing.T) {
	t.Parallel()
	point := filepath.Join(t.TempDir(), "0123")

	plat := &fakePlatform{}
	m, st := newTestMounter(t, untilStopped, plat, newFakeClock(0, false))
	m.plat.supported = "mounting a remote is not supported on plan9"
	if _, err := m.Mount(t.Context(), testRequest(point)); !isUnavailable(err) || len(st.started()) != 0 {
		t.Errorf("unsupported: %v", err)
	}
	if swept, err := m.SweepAll(t.Context(), "/x"); swept != nil || err != nil || m.Sweep(t.Context(), point) != nil {
		t.Errorf("sweeps on an unsupported system: %v, %v", swept, err)
	}

	m, st = newTestMounter(t, untilStopped, &fakePlatform{}, newFakeClock(0, false))
	if _, err := m.Mount(t.Context(), testRequest("relative/0123")); err == nil || len(st.started()) != 0 {
		t.Errorf("relative point: %v", err)
	}

	m, st = newTestMounter(t, untilStopped, &fakePlatform{sweepErr: errors.New("helper failed")}, newFakeClock(0, false))
	if _, err := m.Mount(t.Context(), testRequest(point)); err == nil || isUnavailable(err) || len(st.started()) != 0 {
		t.Errorf("failed sweep: %v", err)
	}

	m, _ = newTestMounter(t, func(_ context.Context, spec proc.Spec) (proc.Result, error) {
		return proc.Result{ExitCode: -1}, &proc.StartError{Path: spec.Path, Err: os.ErrNotExist}
	}, &fakePlatform{}, newFakeClock(0, false))
	if _, err := m.Mount(t.Context(), testRequest(point)); !isUnavailable(err) || !strings.Contains(err.Error(), "start rclone") {
		t.Errorf("start error: %v", err)
	}
}

func isUnavailable(err error) bool {
	me, ok := errors.AsType[*Error](err)
	return ok && me.Unavailable
}

// TestDetect: the configuration, the static checks and the trial decide,
// in that order.
func TestDetect(t *testing.T) {
	t.Parallel()
	opts := func(t *testing.T) DetectOptions {
		dir := t.TempDir()
		return DetectOptions{Trial: true, MountsDir: filepath.Join(dir, "sources", "mount"), ScratchDir: filepath.Join(dir, "scratch")}
	}

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		plat := &fakePlatform{}
		m, st := newTestMounter(t, untilStopped, plat, newFakeClock(1, false))
		o := opts(t)
		o.Off = true
		if ok, reason := m.Detect(t.Context(), o); ok || reason != "remote_sources.mount is off in worker.yaml" || len(plat.recorded()) != 0 || len(st.started()) != 0 {
			t.Errorf("Detect = %v, %q (calls %q)", ok, reason, plat.recorded())
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		t.Parallel()
		m, _ := newTestMounter(t, untilStopped, &fakePlatform{}, newFakeClock(1, false))
		m.plat.supported = "mounting a remote is not supported on plan9"
		if ok, reason := m.Detect(t.Context(), opts(t)); ok || reason != "mounting a remote is not supported on plan9" {
			t.Errorf("Detect = %v, %q", ok, reason)
		}
	})
	t.Run("static checks fail", func(t *testing.T) {
		t.Parallel()
		m, st := newTestMounter(t, untilStopped, &fakePlatform{static: "/dev/fuse is missing"}, newFakeClock(1, false))
		if ok, reason := m.Detect(t.Context(), opts(t)); ok || reason != "/dev/fuse is missing" || len(st.started()) != 0 {
			t.Errorf("Detect = %v, %q", ok, reason)
		}
	})
	t.Run("no trial", func(t *testing.T) {
		t.Parallel()
		m, st := newTestMounter(t, untilStopped, &fakePlatform{}, newFakeClock(1, false))
		o := opts(t)
		o.Trial = false
		if ok, reason := m.Detect(t.Context(), o); !ok || reason != "" || len(st.started()) != 0 {
			t.Errorf("Detect = %v, %q", ok, reason)
		}
	})
	t.Run("trial mounts", func(t *testing.T) {
		t.Parallel()
		plat := &fakePlatform{readyAfter: 1}
		m, st := newTestMounter(t, untilStopped, plat, newFakeClock(1, false))
		o := opts(t)
		if ok, reason := m.Detect(t.Context(), o); !ok || reason != "" {
			t.Fatalf("Detect = %v, %q", ok, reason)
		}
		specs := st.started()
		if len(specs) != 1 {
			t.Fatalf("started %d processes", len(specs))
		}
		args := specs[0].Args
		if n := len(args); n < 3 || !strings.HasPrefix(args[n-2], ":local:"+o.ScratchDir) ||
			args[n-1] != filepath.Join(o.MountsDir, TrialPointName) || !slices.Contains(args, "--read-only") {
			t.Errorf("trial args = %q", args)
		}
		if got := plat.recorded(); !slices.Equal(got, []string{"static", "sweep trial", "prepare trial", "cleanup trial"}) {
			t.Errorf("calls = %q", got)
		}
		// The trial removed its folder.
		if entries, err := os.ReadDir(o.ScratchDir); err != nil || len(entries) != 0 {
			t.Errorf("scratch after the trial = %v, %v", entries, err)
		}
	})
	t.Run("trial fails", func(t *testing.T) {
		t.Parallel()
		m, _ := newTestMounter(t, exits(1,
			`{"level":"notice","msg":"mount helper error: fusermount3: mount failed: Permission denied"}`,
			`{"level":"critical","msg":"Fatal error: failed to mount FUSE fs: fusermount: exit status 1"}`,
		), &fakePlatform{}, newFakeClock(0, false))
		o := opts(t)
		ok, reason := m.Detect(t.Context(), o)
		want := "a trial mount failed: rclone mount failed (exit code 1): mount helper error: fusermount3: mount failed: Permission denied; " +
			"Fatal error: failed to mount FUSE fs: fusermount: exit status 1"
		if ok || reason != want {
			t.Errorf("Detect = %v, %q", ok, reason)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		m, _ := newTestMounter(t, untilStopped, &fakePlatform{}, newFakeClock(0, false))
		o := opts(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if ok, reason := m.Detect(ctx, o); ok || reason != "the worker stopped before it could try a mount" {
			t.Errorf("Detect = %v, %q", ok, reason)
		}
	})
}

func TestReason(t *testing.T) {
	t.Parallel()
	if got := Reason("two\nlines\t and  spaces"); got != "two lines and spaces" {
		t.Errorf("Reason = %q", got)
	}
	long := Reason(strings.Repeat("é", 200))
	if len(long) > MaxReasonBytes || !strings.HasSuffix(long, "...") || !strings.HasPrefix(long, "éé") || strings.Contains(long, "�") {
		t.Errorf("Reason of a long text = %q (%d bytes)", long, len(long))
	}
	if got := Reason("bad \xff byte"); got != "bad � byte" {
		t.Errorf("Reason = %q", got)
	}
}

func TestNoNewPrivs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		status     string
		set, found bool
	}{
		{"Name:\tsyncr-worker\nNoNewPrivs:\t0\nSeccomp:\t0\n", false, true},
		{"Name:\tsyncr-worker\nNoNewPrivs:\t1\n", true, true},
		{"Name:\told-kernel\n", false, false},
		{"", false, false},
	} {
		if set, found := noNewPrivs([]byte(tt.status)); set != tt.set || found != tt.found {
			t.Errorf("noNewPrivs(%q) = %v, %v", tt.status, set, found)
		}
	}
}

// TestParseTable reads a mount table with an rclone mount (recorded on
// Linux 6.x; the device and paths are examples) and escaped paths.
func TestParseTable(t *testing.T) {
	t.Parallel()
	table := strings.Join([]string{
		`22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw`,
		`38 22 0:33 / /var/lib/syncr/sources/mount/0123456789abcdef0123456789abcdef rw,nosuid,nodev,relatime shared:20 - fuse.rclone syncr-0123456789abcdef0123456789abcdef rw,user_id=0,group_id=0`,
		`39 22 0:34 / /var/lib/syncr/sources/mount/trial ro,nosuid,nodev,relatime - fuse.rclone syncr-trial ro,user_id=0,group_id=0`,
		`40 22 0:35 / /srv/with\040space\134and\011tab rw - ext4 /dev/sdb1 rw`,
		`41 38 0:36 / /var/lib/syncr/sources/mount/0123456789abcdef0123456789abcdef/nested rw - tmpfs tmpfs rw`,
		`42 22 0:37 / /var/lib/syncr/sources/mount2/x rw - tmpfs tmpfs rw`,
		`too short`,
	}, "\n")
	infos := ParseTable([]byte(table))
	if len(infos) != 6 {
		t.Fatalf("parsed %d entries: %+v", len(infos), infos)
	}
	if infos[1] != (Entry{Point: "/var/lib/syncr/sources/mount/0123456789abcdef0123456789abcdef", FSType: "fuse.rclone", Source: "syncr-0123456789abcdef0123456789abcdef"}) {
		t.Errorf("rclone entry = %+v", infos[1])
	}
	if infos[3].Point != "/srv/with space\\and\ttab" {
		t.Errorf("escaped point = %q", infos[3].Point)
	}
	if !mountedAt(infos, "/var/lib/syncr/sources/mount/trial") || mountedAt(infos, "/var/lib/syncr/sources/mount") {
		t.Error("mountedAt is wrong")
	}
	got := mountsBelow(infos, "/var/lib/syncr/sources/mount")
	want := []string{"/var/lib/syncr/sources/mount/0123456789abcdef0123456789abcdef", "/var/lib/syncr/sources/mount/trial"}
	if !slices.Equal(got, want) {
		t.Errorf("mountsBelow = %q, want %q", got, want)
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]bool{
		"mount helper error: fusermount3: fuse device not found, try 'modprobe fuse' first": true,
		"Fatal error: failed to mount FUSE fs: fusermount: exit status 1":                   true,
		"cannot open /dev/fuse":                         true,
		"Fatal error: cannot find WinFsp":               true,
		"cgofuse: cannot find winfsp":                   true,
		"Fatal error: failed to create file system: x":  false,
		`"/data/sources/mount/k" is not empty`:          false,
		"error listing: directory not found":            false,
		"Fatal error: failed to mount FUSE fs: timeout": false,
	} {
		if got := unavailable([]string{msg}); got != want {
			t.Errorf("unavailable(%q) = %v, want %v", msg, got, want)
		}
	}
}
