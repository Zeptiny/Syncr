package rclone

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// Two-way sync tests, unit and integration alike, build their folders in
// shortBase: rclone names its state files after both full paths, at most
// 200 bytes together, and t.TempDir is too long for that on Windows
// runners (it nests the test's name under the user's temp directory).

// shortBase returns a new, empty directory with a short path: under
// /dev/shm where it exists (Linux; t.TempDir can be long, and memory is
// fast), else in the system temp directory. It is removed after the test.
func shortBase(t *testing.T) string {
	t.Helper()
	parent := ""
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() && runtime.GOOS == "linux" {
		parent = "/dev/shm"
	}
	dir, err := os.MkdirTemp(parent, "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proc.RemoveAll(dir); err != nil {
			t.Errorf("remove the test directory: %v", err)
		}
	})
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
