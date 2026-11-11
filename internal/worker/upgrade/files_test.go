package upgrade

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOSSwapper(t *testing.T) {
	t.Parallel()
	for _, windows := range []bool{false, true} {
		t.Run(map[bool]string{false: "unix", true: "windows"}[windows], func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			exe, next, backup := filepath.Join(dir, "syncr-worker"), filepath.Join(dir, ".new"), filepath.Join(dir, "syncr-worker.old")
			writeFile(t, exe, "old")
			writeFile(t, next, "new")
			writeFile(t, backup, "a leftover of an earlier upgrade")
			s := OSSwapper{Windows: windows}

			if err := s.Swap(exe, next, backup); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, exe); got != "new" {
				t.Errorf("installed = %q, want new", got)
			}
			if got := readFile(t, backup); got != "old" {
				t.Errorf("backup = %q, want old", got)
			}
			if _, err := os.Stat(next); !os.IsNotExist(err) {
				t.Errorf("the new file is still at its temporary path: %v", err)
			}

			if err := s.Restore(exe, backup); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, exe); got != "old" {
				t.Errorf("restored = %q, want old", got)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Errorf("backup still there after restore: %v", err)
			}
		})
	}
}

func TestOSSwapperFailureLeavesTheBinary(t *testing.T) {
	t.Parallel()
	for _, windows := range []bool{false, true} {
		dir := t.TempDir()
		exe, backup := filepath.Join(dir, "syncr-worker"), filepath.Join(dir, "syncr-worker.old")
		writeFile(t, exe, "old")
		if err := (OSSwapper{Windows: windows}).Swap(exe, filepath.Join(dir, "missing"), backup); err == nil {
			t.Errorf("windows=%v: Swap of a missing file succeeded", windows)
		}
		if got := readFile(t, exe); got != "old" {
			t.Errorf("windows=%v: installed = %q after a failed swap", windows, got)
		}
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Errorf("windows=%v: a failed swap left a backup: %v", windows, err)
		}
	}
}

func TestRemoveLeftover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "syncr-worker")
	writeFile(t, exe, "current")
	writeFile(t, exe+BackupSuffix, "previous")
	now := time.Now()
	stale, fresh := filepath.Join(dir, ".syncr-worker-new-1"), filepath.Join(dir, ".syncr-worker-download-2")
	writeFile(t, stale, "x")
	writeFile(t, fresh, "x")
	if err := os.Chtimes(stale, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := removeLeftover(exe, now); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{exe: true, exe + BackupSuffix: false, stale: false, fresh: true} {
		_, err := os.Stat(path)
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(path), got, want)
		}
	}
	// Nothing to remove is fine.
	if err := RemoveLeftover(exe); err != nil {
		t.Errorf("second RemoveLeftover: %v", err)
	}
}

func TestRemoveLeftoverFollowsSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "real", "syncr-worker")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, exe, "current")
	writeFile(t, exe+BackupSuffix, "previous")
	link := filepath.Join(dir, "syncr-worker")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := RemoveLeftover(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe + BackupSuffix); !os.IsNotExist(err) {
		t.Errorf("backup next to the real binary still there: %v", err)
	}
}
