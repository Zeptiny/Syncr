package upgrade

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
)

// Files next to the installed binary. Released workers rely on these names
// (docs/adr/0016-releases-versioning.md, what is frozen at a tag): a later
// version's start must not remove a backup or a temporary file that an
// upgrade run by an earlier version still needs.
const (
	// BackupSuffix is appended to the installed binary's path for the copy
	// of the previous version an upgrade keeps until it succeeded (e.g.
	// syncr-worker.exe.old).
	BackupSuffix = ".old"
	// LockFileName is the file in the installed binary's directory that an
	// upgrade holds locked (internal/worker/filelock) from its start to its
	// end, so two upgrades never run at once and a starting worker knows
	// that one runs. It stays in place, unlocked, afterwards.
	LockFileName = ".syncr-worker-upgrade.lock"
)

// Swapper replaces the installed binary and puts it back. OSSwapper is the
// implementation; tests may use a fake.
type Swapper interface {
	// Swap puts the file newPath at path and keeps the file that was at
	// path at backup (replacing a leftover backup). If it fails, path is
	// unchanged.
	Swap(path, newPath, backup string) error
	// Restore puts backup back at path, replacing what is there.
	Restore(path, backup string) error
}

// OSSwapper swaps files on the local filesystem.
//
// Unix: the old binary is first hard-linked to backup (copied where links
// are not possible), then newPath is renamed over path: an atomic step, so
// path always names a complete binary, and the running process keeps
// running its old file.
//
// Windows cannot replace or delete a running executable, but it can rename
// one: path is renamed to backup, then newPath to path. Restore renames
// backup back over path once the service that ran the new binary has
// stopped, retrying briefly while Windows still holds the file.
type OSSwapper struct {
	// Windows selects the Windows strategy; NewOSSwapper sets it from the
	// running OS. Both strategies work everywhere, so either can be tested
	// on any OS.
	Windows bool
	// Original, if set, is a file whose content is the binary Swap
	// replaced: on Linux "/proc/self/exe" when the upgrade runs from that
	// binary (the process keeps its file even after the swap and the loss
	// of backup). When backup is missing, Restore installs a copy of it
	// instead, so a rollback works even if something removed the backup.
	Original string
}

// NewOSSwapper returns the OSSwapper for the running OS.
func NewOSSwapper() OSSwapper { return OSSwapper{Windows: runtime.GOOS == "windows"} }

// procSelfExe is the running process's executable on Linux: it opens the
// program's file even after that was renamed or removed.
const procSelfExe = "/proc/self/exe"

// runningOriginal returns procSelfExe when it is the file at exe, the
// binary an upgrade is about to replace, and "" otherwise (another OS, or
// an upgrade not run from the installed binary, as in tests).
func runningOriginal(exe string) string {
	self, err := os.Stat(procSelfExe)
	if err != nil {
		return ""
	}
	installed, err := os.Stat(exe)
	if err != nil || !os.SameFile(self, installed) {
		return ""
	}
	return procSelfExe
}

// Swap implements Swapper.
func (s OSSwapper) Swap(path, newPath, backup string) error {
	if err := os.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the leftover %s: %w", backup, err)
	}
	if s.Windows {
		if err := os.Rename(path, backup); err != nil {
			return fmt.Errorf("move %s aside: %w", path, err)
		}
		if err := os.Rename(newPath, path); err != nil {
			if undo := os.Rename(backup, path); undo != nil {
				return fmt.Errorf("install the new binary: %w; and %s could not be moved back from %s: %w", err, path, backup, undo)
			}
			return fmt.Errorf("install the new binary: %w", err)
		}
		return nil
	}
	if err := linkOrCopy(path, backup); err != nil {
		return fmt.Errorf("keep a copy of %s: %w", path, err)
	}
	if err := os.Rename(newPath, path); err != nil {
		_ = os.Remove(backup)
		return fmt.Errorf("install the new binary: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// Restore implements Swapper. When backup is missing and s.Original is
// set, it installs a copy of s.Original at path instead.
func (s OSSwapper) Restore(path, backup string) error {
	err := os.Rename(backup, path)
	if errors.Is(err, fs.ErrNotExist) && s.Original != "" {
		if err := installCopy(s.Original, path); err != nil {
			return fmt.Errorf("the backup %s is missing, and copying the running %s to %s failed: %w", backup, s.Original, path, err)
		}
		return nil
	}
	if s.Windows {
		// The stopped service's process may hold the file a moment longer.
		for delay := 50 * time.Millisecond; err != nil && delay <= 3200*time.Millisecond; delay *= 2 {
			time.Sleep(delay)
			err = os.Rename(backup, path)
		}
	}
	if err != nil {
		return fmt.Errorf("move %s back to %s: %w", backup, path, err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// installCopy copies src, with its permission bits, to a temporary file
// next to path and renames that over path.
func installCopy(src, path string) (err error) {
	in, err := os.Open(src) //nolint:gosec // the running worker binary
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), tempBinary)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chmod(out.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	if err = os.Rename(out.Name(), path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// linkOrCopy makes dst a hard link to src, or a copy with src's mode where
// the filesystem has no hard links.
func linkOrCopy(src, dst string) (err error) {
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src) //nolint:gosec // the installed worker binary
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) //nolint:gosec // next to the installed binary
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// syncDir flushes a directory's entries (the renames) where the OS can.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // the installed binary's directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// staleAfter is the age after which a temporary file of an upgrade is
// taken to be a leftover of one that was killed, not of one in progress.
const staleAfter = 24 * time.Hour

// RemoveLeftover removes, when the worker starts, what earlier upgrades
// left next to executable and no upgrade can still need. Nothing is removed
// while an upgrade holds LockFileName: the starting worker may be the new
// version that upgrade is watching, and the upgrade's files are what it
// rolls back with.
//
//   - The temporary files of an upgrade that was killed, once they are
//     older than a day.
//   - On Windows only, the backup (executable + BackupSuffix). A
//     successful upgrade cannot remove it there, because the upgrade
//     command runs from that file; so the next start does. While the
//     command runs, its lock is held and Windows refuses to delete the
//     file it runs from, so a start never removes the backup of an
//     upgrade in progress.
//
// On other systems the backup is never removed at start: a successful
// upgrade removes its own, and one an interrupted upgrade left behind
// stays as the copy to roll back to by hand, until the next upgrade
// replaces it. A missing file is not an error.
func RemoveLeftover(executable string) error {
	return removeLeftover(executable, runtime.GOOS == "windows", time.Now())
}

func removeLeftover(executable string, windows bool, now time.Time) error {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	dir := filepath.Dir(executable)
	var leftovers []string
	if windows {
		if info, err := os.Lstat(executable + BackupSuffix); err == nil && info.Mode().IsRegular() {
			leftovers = append(leftovers, executable+BackupSuffix)
		}
	}
	for _, pattern := range []string{tempDownload, tempBinary} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) >= staleAfter {
				leftovers = append(leftovers, m)
			}
		}
	}
	if len(leftovers) == 0 {
		return nil
	}
	release, free, err := filelock.TryLock(filepath.Join(dir, LockFileName))
	if err != nil {
		return fmt.Errorf("check for an upgrade in progress: %w", err)
	}
	if !free {
		return nil // an upgrade runs; it removes its own files
	}
	defer func() { _ = release() }()
	var errs []error
	for _, path := range leftovers {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sameEntry reports whether the paths a and b, with symbolic links
// resolved, name the same directory entry: the same name in the same
// directory. That decides whether replacing b replaces the program a
// service started as a runs. Comparing the strings is not enough (a short
// 8.3 name on Windows, a bind mount), and comparing the files is too much:
// a hard link in another directory is the same file, but replacing b would
// leave it with the old binary. Names are compared case-insensitively on
// Windows. A path that cannot be resolved names no entry.
func sameEntry(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	na, nb := filepath.Base(ra), filepath.Base(rb)
	if na != nb && (runtime.GOOS != "windows" || !strings.EqualFold(na, nb)) {
		return false
	}
	da, err := os.Stat(filepath.Dir(ra))
	if err != nil {
		return false
	}
	db, err := os.Stat(filepath.Dir(rb))
	if err != nil {
		return false
	}
	return os.SameFile(da, db)
}
