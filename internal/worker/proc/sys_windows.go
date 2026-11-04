//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

// configureCmd starts the child in a new process group, so console Ctrl+C
// events reach only the worker, and makes cancellation kill the child
// immediately: Windows has no portable graceful-stop signal for console
// programs.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
}
