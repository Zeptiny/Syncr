//go:build unix

package proc

import (
	"fmt"
	"os/exec"
	"syscall"
)

// printProcessGroup prints the helper's PID and process group ID.
func printProcessGroup() int {
	fmt.Printf("%d %d\n", syscall.Getpid(), syscall.Getpgrp())
	return 0
}

// leaveGroup makes cmd start in a session (and process group) of its own,
// as a daemon would, so it is not part of its parent's process group.
func leaveGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
