package upgrade

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
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

// TestOSSwapperRestoresFromTheOriginal: when the backup is gone, a swapper
// that knows another copy of the old binary (on Linux the upgrade's own
// /proc/self/exe) installs that copy, with its mode; without one, Restore
// fails and changes nothing.
func TestOSSwapperRestoresFromTheOriginal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe, backup, original := filepath.Join(dir, "syncr-worker"), filepath.Join(dir, "syncr-worker.old"), filepath.Join(t.TempDir(), "running")
	writeFile(t, exe, "new")
	writeFile(t, original, "old")

	if err := (OSSwapper{}).Restore(exe, backup); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Restore without a backup or an original = %v, want ErrNotExist", err)
	}
	if got := readFile(t, exe); got != "new" {
		t.Errorf("a failed restore changed the binary to %q", got)
	}

	if err := (OSSwapper{Original: original}).Restore(exe, backup); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, exe); got != "old" {
		t.Errorf("restored = %q, want the original's content", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
			t.Errorf("mode = %o, want the original's 755", fi.Mode().Perm())
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%d entries next to the binary, want only it", len(entries))
	}

	// A backup that is there wins over the original.
	writeFile(t, backup, "backup")
	if err := (OSSwapper{Original: original}).Restore(exe, backup); err != nil || readFile(t, exe) != "backup" {
		t.Errorf("Restore = %v, installed %q; want the backup", err, readFile(t, exe))
	}
}

// TestRunningOriginal: the running process's executable is the fallback
// copy only where /proc/self/exe exists and is the binary to replace.
func TestRunningOriginal(t *testing.T) {
	t.Parallel()
	if got := runningOriginal(filepath.Join(t.TempDir(), "other")); got != "" {
		t.Errorf("runningOriginal(another file) = %q, want none", got)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	if runtime.GOOS == "linux" {
		want = procSelfExe
	}
	if got := runningOriginal(self); got != want {
		t.Errorf("runningOriginal(this test binary) = %q, want %q", got, want)
	}
}

// leftoverFixture is an installed binary with a backup, a stale and a fresh
// temporary file next to it.
type leftoverFixture struct {
	dir, exe, backup, stale, fresh string
	now                            time.Time
}

func newLeftoverFixture(t *testing.T) leftoverFixture {
	t.Helper()
	dir := t.TempDir()
	f := leftoverFixture{
		dir: dir, exe: filepath.Join(dir, "syncr-worker"), now: time.Now(),
		stale: filepath.Join(dir, ".syncr-worker-new-1"), fresh: filepath.Join(dir, ".syncr-worker-download-2"),
	}
	f.backup = f.exe + BackupSuffix
	for _, p := range []string{f.exe, f.backup, f.stale, f.fresh} {
		writeFile(t, p, "x")
	}
	if err := os.Chtimes(f.stale, f.now.Add(-48*time.Hour), f.now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f leftoverFixture) check(t *testing.T, want map[string]bool) {
	t.Helper()
	for path, exists := range want {
		_, err := os.Stat(path)
		if got := err == nil; got != exists {
			t.Errorf("%s exists = %v, want %v", filepath.Base(path), got, exists)
		}
	}
}

// TestRemoveLeftover: a start removes the stale temporary files of a killed
// upgrade everywhere, and the backup only on Windows, where a successful
// upgrade cannot remove it itself. On other systems the backup stays: the
// upgrade removes it once it succeeded, and until then (or after an
// upgrade that was interrupted) it is what a rollback puts back.
func TestRemoveLeftover(t *testing.T) {
	t.Parallel()
	for _, windows := range []bool{false, true} {
		f := newLeftoverFixture(t)
		if err := removeLeftover(f.exe, windows, f.now); err != nil {
			t.Fatal(err)
		}
		f.check(t, map[string]bool{f.exe: true, f.backup: !windows, f.stale: false, f.fresh: true})
		// Nothing (more) to remove is fine.
		if err := removeLeftover(f.exe, windows, f.now); err != nil {
			t.Errorf("windows=%v: second removeLeftover: %v", windows, err)
		}
	}
	// The exported one decides by the running OS.
	f := newLeftoverFixture(t)
	if err := RemoveLeftover(f.exe); err != nil {
		t.Fatal(err)
	}
	f.check(t, map[string]bool{f.backup: runtime.GOOS != "windows", f.stale: false})
}

// TestRemoveLeftoverLeavesARunningUpgradeAlone: while an upgrade holds its
// lock, a starting worker (possibly the new version the upgrade watches)
// removes nothing, on any OS.
func TestRemoveLeftoverLeavesARunningUpgradeAlone(t *testing.T) {
	t.Parallel()
	for _, windows := range []bool{false, true} {
		f := newLeftoverFixture(t)
		release, ok, err := filelock.TryLock(filepath.Join(f.dir, LockFileName))
		if err != nil || !ok {
			t.Fatalf("TryLock = %v, %v", ok, err)
		}
		if err := removeLeftover(f.exe, windows, f.now); err != nil {
			t.Fatal(err)
		}
		f.check(t, map[string]bool{f.exe: true, f.backup: true, f.stale: true, f.fresh: true})
		if err := release(); err != nil {
			t.Fatal(err)
		}
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
	if err := removeLeftover(link, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe + BackupSuffix); !os.IsNotExist(err) {
		t.Errorf("backup next to the real binary still there: %v", err)
	}
}
