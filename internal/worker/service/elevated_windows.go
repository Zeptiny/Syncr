//go:build windows

package service

import "golang.org/x/sys/windows"

// Elevated reports whether the process runs with an elevated (administrator)
// token.
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
