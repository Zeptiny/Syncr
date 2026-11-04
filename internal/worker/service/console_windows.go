//go:build windows

package service

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// HideConsole detaches the process from its console, which closes the console
// window Task Scheduler opens for a console program started at logon. After
// it, nothing can be written to stdout or stderr: log to a file.
func HideConsole() error {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
	if r, _, err := proc.Call(); r == 0 {
		return fmt.Errorf("detach from the console: %w", err)
	}
	return nil
}
