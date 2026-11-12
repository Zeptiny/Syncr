//go:build !windows

package atomicfile

import "os"

func isWindows() bool { return false }

// createTemp creates Write's temporary file. os.CreateTemp makes it 0600, so
// it is private from the start; Write applies Options.Perm afterwards.
func createTemp(dir, base string, _ Options) (f *os.File, aclWarning, err error) {
	f, err = os.CreateTemp(dir, tempPattern(base))
	return f, nil, err
}

// syncDir makes the rename durable; failures are not actionable.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // the directory of a path the caller chose
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
