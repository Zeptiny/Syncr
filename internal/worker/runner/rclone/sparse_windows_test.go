//go:build integration && windows

package rclone

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// markSparse makes f a sparse file (FSCTL_SET_SPARSE), so that extending it
// with Truncate allocates no disk space.
func markSparse(f *os.File) error {
	var returned uint32
	err := windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &returned, nil)
	if err != nil {
		return fmt.Errorf("FSCTL_SET_SPARSE: %w", err)
	}
	return nil
}
