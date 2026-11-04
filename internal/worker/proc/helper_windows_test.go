//go:build windows

package proc

// printProcessGroup is a no-op on Windows, which has no process group IDs
// comparable to Unix ones.
func printProcessGroup() int { return 0 }
