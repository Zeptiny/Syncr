//go:build !windows

package service

// HideConsole does nothing outside Windows.
func HideConsole() error { return nil }
