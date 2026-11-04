//go:build !windows

package atomicfile

import "os"

func isWindows() bool { return false }

// restrictACL does nothing: Unix permission bits are applied by Write.
func restrictACL(string, bool) error { return nil }

// syncDir makes the rename durable; failures are not actionable.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // the directory of a path the caller chose
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
