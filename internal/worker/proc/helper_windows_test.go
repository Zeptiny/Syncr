//go:build windows

package proc

import "os/exec"

// printProcessGroup is a no-op on Windows, which has no process group IDs
// comparable to Unix ones.
func printProcessGroup() int { return 0 }

// leaveGroup does nothing on Windows: a child cannot leave its parent's Job
// Object unless the job allows it, and the worker's do not.
func leaveGroup(*exec.Cmd) {}
