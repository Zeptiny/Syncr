//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes flock(2) on path. flock locks belong to the open file
// description, so they also exclude other goroutines of this process.
func tryLock(path string) (func() error, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the caller chooses the lock file
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Closing the file releases the lock.
	return f.Close, true, nil
}
