//go:build linux

package mount

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// tableLine is a mount table line for a FUSE mount at point.
func tableLine(point string) string {
	return "38 22 0:33 / " + strings.ReplaceAll(point, " ", `\040`) + " rw,nosuid,nodev - fuse.rclone syncr-x rw,user_id=0,group_id=0\n"
}

// fakeLinux is a linuxMount over a mount table the test sets and a FUSE
// helper that removes the point from the table.
type fakeLinux struct {
	l       *linuxMount
	table   []string
	helpers []string
	runs    [][]string
	failRun bool
}

func newFakeLinux(t *testing.T) *fakeLinux {
	t.Helper()
	f := &fakeLinux{helpers: []string{"fusermount3"}}
	st := &fakeStarter{run: func(_ context.Context, spec proc.Spec) (proc.Result, error) {
		f.runs = append(f.runs, append([]string{filepath.Base(spec.Path)}, spec.Args...))
		if f.failRun {
			spec.Stderr(proc.Line{Data: []byte("fusermount3: entry for /x not found in /etc/mtab")})
			return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
		}
		point := spec.Args[len(spec.Args)-1]
		escaped := strings.ReplaceAll(point, " ", `\040`)
		f.table = slices.DeleteFunc(f.table, func(l string) bool { return strings.Contains(l, " "+escaped+" ") })
		return proc.Result{}, nil
	}}
	m, err := New(Config{Rclone: "/fake/rclone", Supervisor: st})
	if err != nil {
		t.Fatal(err)
	}
	f.l = &linuxMount{
		m:             m,
		readMountInfo: func() ([]byte, error) { return []byte(strings.Join(f.table, "")), nil },
		lookPath: func(name string) (string, error) {
			if slices.Contains(f.helpers, name) {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
	}
	return f
}

// TestLinuxSweep: a point the mount table lists is unmounted lazily with
// the helper; one it does not list is left alone (and never stat'ed).
func TestLinuxSweep(t *testing.T) {
	t.Parallel()
	f := newFakeLinux(t)
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(dir, "0123")
	if err := f.l.sweep(t.Context(), point); err != nil || len(f.runs) != 0 {
		t.Fatalf("sweep of an unmounted point: %v, runs %q", err, f.runs)
	}
	f.table = []string{tableLine(filepath.Join(resolved, "0123"))}
	if err := f.l.sweep(t.Context(), point); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"fusermount3", "-u", "-z", filepath.Join(resolved, "0123")}}; !slices.EqualFunc(f.runs, want, slices.Equal) {
		t.Errorf("runs = %q", f.runs)
	}

	// fusermount when fusermount3 is missing; neither: an error.
	f.helpers = []string{"fusermount"}
	f.table = []string{tableLine(filepath.Join(resolved, "0123"))}
	if err := f.l.sweep(t.Context(), point); err != nil || f.runs[len(f.runs)-1][0] != "fusermount" {
		t.Errorf("sweep with fusermount: %v, runs %q", err, f.runs)
	}
	f.helpers = nil
	f.table = []string{tableLine(filepath.Join(resolved, "0123"))}
	if err := f.l.sweep(t.Context(), point); err == nil || !strings.Contains(err.Error(), "fuse3 package") {
		t.Errorf("sweep without a helper: %v", err)
	}
}

// TestLinuxCleanup: after rclone ended, a point still mounted is unmounted
// lazily; one the helper cannot unmount is an error.
func TestLinuxCleanup(t *testing.T) {
	t.Parallel()
	f := newFakeLinux(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(dir, "0123")
	if err := f.l.cleanup(t.Context(), point); err != nil || len(f.runs) != 0 {
		t.Errorf("cleanup of an unmounted point: %v, %q", err, f.runs)
	}
	f.table = []string{tableLine(point)}
	if err := f.l.cleanup(t.Context(), point); err != nil || len(f.runs) != 1 || len(f.table) != 0 {
		t.Errorf("cleanup: %v, runs %q, table %q", err, f.runs, f.table)
	}
	f.table, f.failRun = []string{tableLine(point)}, true
	if err := f.l.cleanup(t.Context(), point); err == nil || !strings.Contains(err.Error(), "not found in /etc/mtab") {
		t.Errorf("failed cleanup: %v", err)
	}
	if ok, err := f.l.mounted(point); !ok || err != nil {
		t.Errorf("mounted after a failed cleanup = %v, %v", ok, err)
	}
}

// TestLinuxSweepAll unmounts every mount directly in the folder of mount
// points, and nothing else; it never lists the folder.
func TestLinuxSweepAll(t *testing.T) {
	t.Parallel()
	f := newFakeLinux(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "sources", "mount")
	if swept, err := f.l.sweepAll(t.Context(), dir); swept != nil || err != nil {
		t.Errorf("sweepAll without the folder = %q, %v", swept, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A link to the folder sweeps the same mounts (the table names
	// resolved paths).
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	f.table = []string{
		tableLine(filepath.Join(dir, "0123456789abcdef0123456789abcdef")),
		tableLine(filepath.Join(dir, "trial")),
		tableLine(filepath.Join(dir, "trial", "nested")),
		tableLine(filepath.Join(base, "elsewhere")),
		tableLine(filepath.Join(dir, "with space")),
	}
	swept, err := f.l.sweepAll(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "0123456789abcdef0123456789abcdef"), filepath.Join(dir, "trial"), filepath.Join(dir, "with space")}
	if !slices.Equal(swept, want) {
		t.Errorf("swept %q, want %q", swept, want)
	}
	if len(f.table) != 2 {
		t.Errorf("left in the table: %q", f.table)
	}
}

func TestResolvePoint(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if got := resolvePoint(filepath.Join(base, "link", "k")); got != filepath.Join(base, "real", "k") {
		t.Errorf("resolvePoint = %q", got)
	}
	if got := resolvePoint("/does/not/exist/k"); got != "/does/not/exist/k" {
		t.Errorf("resolvePoint of a missing parent = %q", got)
	}
}

// fakeInfo is a file's mode and owner.
type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "fusermount3" }
func (f fakeInfo) Size() int64        { return 39296 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

// TestLinuxChecks: the static checks with fakes for each outcome.
func TestLinuxChecks(t *testing.T) {
	t.Parallel()
	setuid := fakeInfo{mode: 0o755 | fs.ModeSetuid}
	ok := func() linuxChecks {
		return linuxChecks{
			openFuse:   func() error { return nil },
			lookPath:   func(name string) (string, error) { return "/usr/bin/" + name, nil },
			stat:       func(string) (fs.FileInfo, error) { return setuid, nil },
			geteuid:    func() int { return 10001 },
			readStatus: func() ([]byte, error) { return []byte("Name:\tsyncr-worker\nNoNewPrivs:\t0\n"), nil },
		}
	}
	for name, tt := range map[string]struct {
		change func(*linuxChecks)
		want   string
	}{
		"everything allows it": {func(*linuxChecks) {}, ""},
		"no /dev/fuse": {func(c *linuxChecks) {
			c.openFuse = func() error { return &fs.PathError{Op: "open", Path: "/dev/fuse", Err: syscall.ENOENT} }
		}, "/dev/fuse is missing"},
		"/dev/fuse not allowed": {func(c *linuxChecks) {
			c.openFuse = func() error { return &fs.PathError{Op: "open", Path: "/dev/fuse", Err: syscall.EPERM} }
		}, "/dev/fuse cannot be opened: permission denied (a container needs the device and the right to use it)"},
		"/dev/fuse other error": {func(c *linuxChecks) {
			c.openFuse = func() error { return syscall.EIO }
		}, "/dev/fuse cannot be opened: input/output error"},
		"no helper": {func(c *linuxChecks) {
			c.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
		}, "fusermount3 is not installed (install the fuse3 package)"},
		"only fusermount": {func(c *linuxChecks) {
			c.lookPath = func(name string) (string, error) {
				if name == "fusermount" {
					return "/bin/fusermount", nil
				}
				return "", exec.ErrNotFound
			}
		}, ""},
		"root needs no setuid": {func(c *linuxChecks) {
			c.geteuid = func() int { return 0 }
			c.stat = func(string) (fs.FileInfo, error) { return fakeInfo{mode: 0o755}, nil }
			c.readStatus = func() ([]byte, error) { return []byte("NoNewPrivs:\t1\n"), nil }
		}, ""},
		"helper not setuid": {func(c *linuxChecks) {
			c.stat = func(string) (fs.FileInfo, error) { return fakeInfo{mode: 0o755}, nil }
		}, "fusermount3 is not setuid root, which a worker that does not run as root needs to mount"},
		"helper setuid but not root's": {func(c *linuxChecks) {
			c.stat = func(string) (fs.FileInfo, error) { return fakeInfo{mode: 0o755 | fs.ModeSetuid, uid: 1000}, nil }
		}, "fusermount3 is not setuid root, which a worker that does not run as root needs to mount"},
		"NoNewPrivileges": {func(c *linuxChecks) {
			c.readStatus = func() ([]byte, error) { return []byte("Name:\tsyncr-worker\nNoNewPrivs:\t1\n"), nil }
		}, "the worker runs with NoNewPrivileges, which keeps fusermount3 from mounting (the systemd user service sets it; see docs/setup-linux.md)"},
		"status unreadable": {func(c *linuxChecks) {
			c.readStatus = func() ([]byte, error) { return nil, fs.ErrPermission }
		}, "/proc/self/status cannot be read: permission denied"},
	} {
		c := ok()
		tt.change(&c)
		if got := c.check(); got != tt.want {
			t.Errorf("%s: check() = %q, want %q", name, got, tt.want)
		}
	}
}

// TestStaticChecksOnThisMachine runs the real checks. On a machine without
// /dev/fuse (such as an LXC container) the reason must say so.
func TestStaticChecksOnThisMachine(t *testing.T) {
	t.Parallel()
	reason := osChecks(exec.LookPath).check()
	t.Logf("static checks on this machine: %q", reason)
	if _, err := os.Stat(fuseDevice); errors.Is(err, fs.ErrNotExist) && reason != "/dev/fuse is missing" {
		t.Errorf("without /dev/fuse the reason is %q", reason)
	}
}

// TestDetectOnThisMachine: without /dev/fuse, Detect reports it and runs no
// trial.
func TestDetectOnThisMachine(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(fuseDevice); !errors.Is(err, fs.ErrNotExist) {
		t.Skip("this machine has /dev/fuse; the mount integration tests cover it")
	}
	st := &fakeStarter{run: untilStopped}
	m, err := New(Config{Rclone: "/fake/rclone", Supervisor: st})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ok, reason := m.Detect(t.Context(), DetectOptions{Trial: true, MountsDir: filepath.Join(dir, "mount"), ScratchDir: dir})
	if ok || reason != "/dev/fuse is missing" || len(st.started()) != 0 {
		t.Errorf("Detect = %v, %q (started %d)", ok, reason, len(st.started()))
	}
}
