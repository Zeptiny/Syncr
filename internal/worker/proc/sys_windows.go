//go:build windows

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the child gets no console window. The
// worker captures all output through pipes, and when it runs without a console
// (as a hidden scheduled task) a console child would otherwise open a window
// for every job.
const createNoWindow = 0x08000000

// configureCmd starts the child in a new process group, so console Ctrl+C
// events reach only the worker, without a console window, and makes
// cancellation kill the child immediately, with every process it started
// (the run's Job Object, see tree): Windows has no portable graceful-stop
// signal for console programs.
func configureCmd(cmd *exec.Cmd, t *tree) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
	cmd.Cancel = func() error { return t.cancel(cmd) }
}

// transientRemoveError reports whether a failed removal may succeed if
// retried, once other processes close their handles: these are the errors
// such a handle causes while it is open. A directory whose file was deleted
// while still open stays "not empty" until that handle is closed.
func transientRemoveError(err error) bool {
	transient := []syscall.Errno{
		syscall.ERROR_ACCESS_DENIED,
		32, // ERROR_SHARING_VIOLATION
		33, // ERROR_LOCK_VIOLATION
		syscall.ERROR_DIR_NOT_EMPTY,
	}
	for _, errno := range transient {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
