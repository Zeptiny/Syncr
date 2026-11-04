//go:build !windows

package service

import "os"

// Elevated reports whether the process runs as root.
func Elevated() bool { return os.Geteuid() == 0 }
