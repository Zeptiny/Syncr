//go:build linux

package proc

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// watched is a process the test observes through a pidfd, which keeps
// naming that process even after it exits.
type watched struct {
	pid int
	fd  int // -1 if the process was already gone
}

// watch opens a pidfd for pid and kills the process at the end of the test
// should it still run.
func watch(t *testing.T, pid int) *watched {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return &watched{pid: pid, fd: -1} // already exited and reaped
	}
	if err != nil {
		t.Fatalf("pidfd_open(%d): %v", pid, err)
	}
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
	})
	return &watched{pid: pid, fd: fd}
}

// gone waits up to timeout for the process to exit and reports whether it
// did. A pidfd becomes readable when its process exits.
func (w *watched) gone(timeout time.Duration) bool {
	if w.fd < 0 {
		return true
	}
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, int(timeout.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n == 1
	}
}
