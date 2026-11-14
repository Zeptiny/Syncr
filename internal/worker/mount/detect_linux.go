//go:build linux

package mount

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// fuseDevice is the FUSE device, which every mount opens.
const fuseDevice = "/dev/fuse"

// linuxChecks are the static checks of Detect on Linux. Each function is a
// seam for tests.
type linuxChecks struct {
	// openFuse opens /dev/fuse read-write and closes it again.
	openFuse func() error
	// lookPath finds a program on PATH.
	lookPath func(file string) (string, error)
	// stat stats a file (the FUSE helper).
	stat func(path string) (fs.FileInfo, error)
	// geteuid returns the effective user ID.
	geteuid func() int
	// readStatus returns /proc/self/status.
	readStatus func() ([]byte, error)
}

// osChecks returns the checks of this process.
func osChecks(lookPath func(string) (string, error)) linuxChecks {
	return linuxChecks{
		openFuse: func() error {
			f, err := os.OpenFile(fuseDevice, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		},
		lookPath:   lookPath,
		stat:       os.Stat,
		geteuid:    os.Geteuid,
		readStatus: func() ([]byte, error) { return os.ReadFile("/proc/self/status") },
	}
}

// check runs the checks in order and returns why mounting cannot work, or
// "" when it may.
func (c linuxChecks) check() string {
	if err := c.openFuse(); err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fuseDevice + " is missing"
		case errors.Is(err, fs.ErrPermission):
			return fuseDevice + " cannot be opened: permission denied (a container needs the device and the right to use it)"
		}
		return fmt.Sprintf("%s cannot be opened: %v", fuseDevice, err)
	}
	helper := ""
	for _, name := range fuseHelpers() {
		if path, err := c.lookPath(name); err == nil {
			helper = path
			break
		}
	}
	if helper == "" {
		return "fusermount3 is not installed (install the fuse3 package)"
	}
	if c.geteuid() == 0 {
		return "" // root mounts without the helper's privileges
	}
	info, err := c.stat(helper)
	if err != nil {
		return fmt.Sprintf("%s cannot be checked: %v", helper, err)
	}
	if !setuidRoot(info) {
		return fmt.Sprintf("%s is not setuid root, which a worker that does not run as root needs to mount", filepath.Base(helper))
	}
	status, err := c.readStatus()
	if err != nil {
		return fmt.Sprintf("/proc/self/status cannot be read: %v", err)
	}
	if set, _ := noNewPrivs(status); set {
		return fmt.Sprintf("the worker runs with NoNewPrivileges, which keeps %s from mounting "+
			"(the systemd user service sets it; see docs/setup-linux.md)", filepath.Base(helper))
	}
	return ""
}

// setuidRoot reports whether info is a file owned by root with the setuid
// bit, so it runs as root whoever starts it.
func setuidRoot(info fs.FileInfo) bool {
	if info.Mode()&fs.ModeSetuid == 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
