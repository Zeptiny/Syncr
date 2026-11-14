//go:build windows

package mount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// windowsMount is the Windows platform (WinFsp): rclone creates the mount
// point itself, a folder that must not exist yet, under a parent that
// does; a mount is ready when the folder exists; WinFsp removes a mount
// whose rclone process exited, so the only leftover is an empty folder.
type windowsMount struct{}

func newPlatform(*Mounter) platform {
	w := windowsMount{}
	return platform{static: osChecks().check, sweep: w.sweep, prepare: w.prepare, mounted: w.mounted, cleanup: w.cleanup, sweepAll: w.sweepAll}
}

// sweep removes an empty folder left at point.
func (windowsMount) sweep(_ context.Context, point string) error {
	_, err := removeEmpty(point)
	return err
}

// prepare creates the parent of point and checks that point itself does
// not exist (rclone mounts only at a folder it creates).
func (windowsMount) prepare(point string) error {
	if err := os.MkdirAll(filepath.Dir(point), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(point); err == nil {
		return fmt.Errorf("%s exists and is not an empty folder; rclone mounts only at a folder that does not exist", point)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (windowsMount) mounted(point string) (bool, error) {
	info, err := os.Stat(point)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return info.IsDir(), nil
}

// cleanup removes the mount point if it is left as an empty folder.
func (windowsMount) cleanup(_ context.Context, point string) error {
	_, err := removeEmpty(point)
	return err
}

// sweepAll removes the empty folders left in dir.
func (windowsMount) sweepAll(_ context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		point := filepath.Join(dir, e.Name())
		ok, err := removeEmpty(point)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed = append(removed, point)
		}
	}
	return removed, errors.Join(errs...)
}

// removeEmpty removes point if it is an empty folder and reports whether
// it did. A folder that holds something is left alone and reported.
func removeEmpty(point string) (bool, error) {
	entries, err := os.ReadDir(point)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case len(entries) > 0:
		return false, fmt.Errorf("%s is left over from an earlier mount and is not empty; remove it", point)
	}
	if err := os.Remove(point); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
