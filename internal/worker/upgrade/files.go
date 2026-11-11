package upgrade

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// BackupSuffix is appended to the installed binary's path for the copy of
// the previous version an upgrade keeps until it succeeded (e.g.
// syncr-worker.exe.old).
const BackupSuffix = ".old"

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
}

// NewOSSwapper returns the OSSwapper for the running OS.
func NewOSSwapper() OSSwapper { return OSSwapper{Windows: runtime.GOOS == "windows"} }

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

// Restore implements Swapper.
func (s OSSwapper) Restore(path, backup string) error {
	err := os.Rename(backup, path)
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

// RemoveLeftover removes the backup an earlier upgrade kept next to
// executable (executable + BackupSuffix), if there is one, and the
// temporary files of an upgrade that was killed (older than a day, so an
// upgrade in progress keeps its own). Call it when the worker starts: on
// Windows the backup of a successful upgrade often cannot be removed at
// once, because the upgrade command itself ran from it. Removing it while
// that command still runs fails, which is harmless: the next start or
// upgrade tries again. A missing backup is not an error.
func RemoveLeftover(executable string) error {
	return removeLeftover(executable, time.Now())
}

func removeLeftover(executable string, now time.Time) error {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	var errs []error
	if err := os.Remove(executable + BackupSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	dir := filepath.Dir(executable)
	for _, pattern := range []string{tempDownload, tempBinary} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if info, err := os.Lstat(m); err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < staleAfter {
				continue
			}
			if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
