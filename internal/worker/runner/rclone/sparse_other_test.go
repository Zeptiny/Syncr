//go:build integration && !windows

package rclone

import "os"

// markSparse is a no-op: Unix filesystems create sparse files when a file is
// extended with Truncate.
func markSparse(*os.File) error { return nil }
