//go:build unix

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// configureCmd places the child in its own process group, so terminal
// signals (Ctrl-C) reach only the worker, and makes cancellation send
// SIGINT: rclone and restic treat it as a user abort and clean up (finish or
// remove partial uploads, release repository locks). exec kills the child
// with SIGKILL if it is still running after WaitDelay.
func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Signal the child itself through os.Process, which tracks whether it
		// was already reaped, so a recycled PID is never signaled.
		return cmd.Process.Signal(os.Interrupt)
	}
}
