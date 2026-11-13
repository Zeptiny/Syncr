//go:build !linux

package localfs

// pseudoFS reports whether the opened folder d is on a pseudo filesystem:
// only Linux has those (see pseudo_linux.go).
func pseudoFS(dirReader) bool { return false }
