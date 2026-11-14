//go:build linux

package mount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// mountInfoPath is the mount table of the worker's own mount namespace.
const mountInfoPath = "/proc/self/mountinfo"

// fuseHelpers are the FUSE helpers, in the order rclone (and this package)
// looks for them.
func fuseHelpers() []string { return []string{"fusermount3", "fusermount"} }

// linuxMount is the Linux platform: mount points are folders that exist and
// are empty, a mount is ready when the mount table lists it, and stale
// mounts are unmounted lazily with the FUSE helper.
type linuxMount struct {
	m *Mounter
	// readMountInfo returns the mount table; tests replace it.
	readMountInfo func() ([]byte, error)
	// lookPath finds a program on PATH; tests replace it.
	lookPath func(file string) (string, error)
}

func newPlatform(m *Mounter) platform {
	l := &linuxMount{
		m:             m,
		readMountInfo: func() ([]byte, error) { return os.ReadFile(mountInfoPath) },
		lookPath:      exec.LookPath,
	}
	return l.platform()
}

func (l *linuxMount) platform() platform {
	c := osChecks(l.lookPath)
	return platform{static: c.check, sweep: l.sweep, prepare: l.prepare, mounted: l.mounted, cleanup: l.cleanup, sweepAll: l.sweepAll}
}

// table reads the mount table.
func (l *linuxMount) table() ([]Entry, error) {
	data, err := l.readMountInfo()
	if err != nil {
		return nil, fmt.Errorf("read the mount table: %w", err)
	}
	return ParseTable(data), nil
}

// resolvePoint returns point as the mount table names it: its parent
// folder with symbolic links resolved (the parent is the worker's folder
// of mount points, never a mount itself, so resolving it is safe), and its
// own name. A parent that does not exist leaves point as it is.
func resolvePoint(point string) string {
	point = filepath.Clean(point)
	parent, err := filepath.EvalSymlinks(filepath.Dir(point))
	if err != nil {
		return point
	}
	return filepath.Join(parent, filepath.Base(point))
}

func (l *linuxMount) mounted(point string) (bool, error) {
	infos, err := l.table()
	if err != nil {
		return false, err
	}
	return mountedAt(infos, resolvePoint(point)), nil
}

// sweep unmounts a mount left at point. It reads only the mount table: a
// stale mount (rclone killed) answers ENOTCONN, and one whose rclone hangs
// blocks whoever stats it, so point is not touched before it is unmounted.
func (l *linuxMount) sweep(ctx context.Context, point string) error {
	ok, err := l.mounted(point)
	if err != nil || !ok {
		return err
	}
	l.m.cfg.Logger.Warn("unmounting a remote that an earlier run left mounted", "mount_point", point)
	return l.unmount(ctx, resolvePoint(point))
}

// prepare creates the mount point (mode 0700). rclone refuses a folder
// that is not empty.
func (l *linuxMount) prepare(point string) error {
	return os.MkdirAll(point, 0o700)
}

// cleanup unmounts point lazily if it is still mounted after rclone ended
// (rclone was killed before it could unmount), and fails if it stays
// mounted.
func (l *linuxMount) cleanup(ctx context.Context, point string) error {
	ok, err := l.mounted(point)
	if err != nil || !ok {
		return err
	}
	if err := l.unmount(ctx, resolvePoint(point)); err != nil {
		return err
	}
	if ok, err := l.mounted(point); err != nil || ok {
		return errors.Join(err, fmt.Errorf("%s is still mounted", point))
	}
	return nil
}

// sweepAll unmounts every mount the mount table lists directly in dir.
func (l *linuxMount) sweepAll(ctx context.Context, dir string) ([]string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve the folder of mount points: %w", err)
	}
	infos, err := l.table()
	if err != nil {
		return nil, err
	}
	var swept []string
	var errs []error
	for _, point := range mountsBelow(infos, resolved) {
		if err := l.unmount(ctx, point); err != nil {
			errs = append(errs, err)
			continue
		}
		swept = append(swept, point)
	}
	return swept, errors.Join(errs...)
}

// unmount unmounts point lazily with the FUSE helper ("fusermount3 -u -z",
// or fusermount): the kernel detaches the mount at once, even when rclone
// is gone or hangs, and frees it once nothing uses it any more.
func (l *linuxMount) unmount(ctx context.Context, point string) error {
	helper, err := l.helper()
	if err != nil {
		return err
	}
	var tail []string
	keep := func(line proc.Line) {
		if s := strings.TrimSpace(string(line.Data)); s != "" && len(tail) < 3 {
			tail = append(tail, s)
		}
	}
	_, err = l.m.cfg.Supervisor.Run(ctx, proc.Spec{Path: helper, Args: []string{"-u", "-z", point}, Stdout: keep, Stderr: keep})
	if err != nil {
		msg := fmt.Sprintf("unmount %s with %s", point, filepath.Base(helper))
		if len(tail) > 0 {
			msg += ": " + strings.Join(tail, "; ")
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	return nil
}

// helper finds the FUSE helper on PATH.
func (l *linuxMount) helper() (string, error) {
	for _, name := range fuseHelpers() {
		if path, err := l.lookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("neither fusermount3 nor fusermount is installed (the fuse3 package)")
}
