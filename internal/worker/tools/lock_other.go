//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package tools

import "errors"

// tryLock is not implemented for this platform; Syncr workers target Linux
// and Windows.
func tryLock(string) (func() error, bool, error) {
	return nil, false, errors.New("file locking is not supported on this platform")
}
