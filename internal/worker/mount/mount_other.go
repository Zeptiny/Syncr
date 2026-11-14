//go:build !linux && !windows

package mount

import "runtime"

// newPlatform: mounting is supported on Linux (FUSE 3) and Windows
// (WinFsp) only.
func newPlatform(*Mounter) platform {
	return platform{supported: "mounting a remote is not supported on " + runtime.GOOS}
}
