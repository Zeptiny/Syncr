//go:build windows

package filelock

import (
	"errors"
	"syscall"
)

// Windows error codes for a file another handle holds exclusively.
const (
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
)

// tryLock opens path with a share mode of zero, which makes every other open
// of the file (in this or another process) fail with a sharing violation
// until the handle is closed. Windows also closes the handle if the process
// dies.
func tryLock(path string) (func() error, bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() error { return syscall.CloseHandle(h) }, true, nil
}
