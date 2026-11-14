//go:build windows

package mount

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// TestWindowsMountPoint: a leftover empty folder is swept, the point must
// not exist before rclone mounts, and a mount is ready once the folder
// exists.
func TestWindowsMountPoint(t *testing.T) {
	t.Parallel()
	w := windowsMount{}
	dir := filepath.Join(t.TempDir(), "sources", "mount")
	point := filepath.Join(dir, "0123456789abcdef0123456789abcdef")
	if err := w.prepare(point); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the parent was not created: %v", err)
	}
	if ok, err := w.mounted(point); ok || err != nil {
		t.Errorf("mounted before rclone = %v, %v", ok, err)
	}
	if err := os.Mkdir(point, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := w.mounted(point); !ok || err != nil {
		t.Errorf("mounted = %v, %v", ok, err)
	}
	if err := w.prepare(point); err == nil {
		t.Error("prepare accepted an existing folder")
	}
	if err := w.sweep(t.Context(), point); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(point); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the empty leftover was kept: %v", err)
	}

	// A leftover that holds something is reported, not removed.
	if err := os.MkdirAll(filepath.Join(point, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.cleanup(t.Context(), point); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("cleanup of a full leftover: %v", err)
	}
	empty := filepath.Join(dir, "trial")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	swept, err := w.sweepAll(t.Context(), dir)
	if !slices.Equal(swept, []string{empty}) || err == nil {
		t.Errorf("sweepAll = %q, %v", swept, err)
	}
}

func TestWindowsChecks(t *testing.T) {
	t.Parallel()
	stat := func(path string) (fs.FileInfo, error) {
		if path == `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll` {
			return os.Stat(os.Args[0])
		}
		return nil, fs.ErrNotExist
	}
	for name, tt := range map[string]struct {
		c    windowsChecks
		want string
	}{
		"installed": {windowsChecks{installDir: func() (string, error) { return `C:\Program Files (x86)\WinFsp\`, nil }, stat: stat, arch: "amd64"}, ""},
		"no key":    {windowsChecks{installDir: func() (string, error) { return "", registry.ErrNotExist }, stat: stat, arch: "amd64"}, "WinFsp is not installed"},
		"empty dir": {windowsChecks{installDir: func() (string, error) { return "", nil }, stat: stat, arch: "amd64"}, "WinFsp is not installed"},
		"no DLL for arm64": {windowsChecks{installDir: func() (string, error) { return `C:\Program Files (x86)\WinFsp\`, nil }, stat: stat, arch: "arm64"},
			`WinFsp is not installed completely: C:\Program Files (x86)\WinFsp\bin\winfsp-a64.dll is missing`},
		"registry error": {windowsChecks{installDir: func() (string, error) { return "", fs.ErrPermission }, stat: stat, arch: "amd64"},
			"WinFsp's installation cannot be read from the registry: permission denied"},
	} {
		if got := tt.c.check(); got != tt.want {
			t.Errorf("%s: check() = %q, want %q", name, got, tt.want)
		}
	}
}

// TestWinFspOnThisMachine logs what the real check finds.
func TestWinFspOnThisMachine(t *testing.T) {
	t.Parallel()
	t.Logf("WinFsp check on this machine: %q", osChecks().check())
}
