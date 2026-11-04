//go:build unix

package proc

import (
	"fmt"
	"syscall"
)

// printProcessGroup prints the helper's PID and process group ID.
func printProcessGroup() int {
	fmt.Printf("%d %d\n", syscall.Getpid(), syscall.Getpgrp())
	return 0
}
