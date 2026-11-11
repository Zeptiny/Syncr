//go:build windows

package proc

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// watched is a process the test observes through a handle, which keeps
// naming that process even after it exits.
type watched struct {
	pid int
	h   windows.Handle // zero if the process was already gone
}

// watch opens a handle to pid and terminates the process at the end of the
// test should it still run.
func watch(t *testing.T, pid int) *watched {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return &watched{pid: pid} // no such process any more
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	})
	return &watched{pid: pid, h: h}
}

// gone waits up to timeout for the process to exit and reports whether it
// did.
func (w *watched) gone(timeout time.Duration) bool {
	if w.h == 0 {
		return true
	}
	ev, err := windows.WaitForSingleObject(w.h, uint32(timeout.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}
