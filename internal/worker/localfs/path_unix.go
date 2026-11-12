//go:build !windows

package localfs

import "path/filepath"

// checkPlatform accepts every clean absolute path: on Unix any byte except
// NUL is valid in a path.
func checkPlatform(string) error { return nil }

// resolveExisting resolves every symbolic link in path, which must exist.
func resolveExisting(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// nameEqual compares volume names and path components; Unix names are
// case-sensitive.
func nameEqual(a, b string) bool { return a == b }
