//go:build windows

package localfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// checkPlatform rejects Windows paths that are absolute but must not be used:
// device and verbatim paths (\\.\, \\?\), which bypass normalization and
// can name raw devices, and components that are reserved device names (NUL,
// CON, COM1...) or contain a colon (alternate data streams).
func checkPlatform(clean string) error {
	vol := filepath.VolumeName(clean)
	if strings.HasPrefix(vol, `\\.\`) || strings.HasPrefix(vol, `\\?\`) || strings.HasPrefix(vol, `\??\`) {
		return errors.New("device and verbatim paths are not supported")
	}
	for _, c := range components(clean[len(vol):]) {
		if !filepath.IsLocal(c) {
			return fmt.Errorf("component %q is a reserved name or contains ':'", c)
		}
	}
	return nil
}

// volumeNameDOS asks GetFinalPathNameByHandle for a drive-letter (or UNC)
// path rather than a volume GUID path.
const volumeNameDOS = 0x0

// resolveExisting returns the final path of the existing file or directory
// path, with every symbolic link, junction and mount point resolved.
// filepath.EvalSymlinks does not resolve junctions and mount points since Go
// 1.23, so the OS is asked for the path of the opened handle instead.
func resolveExisting(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	// Zero access rights are enough to query the name and work without read
	// permission; FILE_FLAG_BACKUP_SEMANTICS allows opening directories.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &winPathError{op: "open", path: path, err: err}
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle; nothing to flush

	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS) //nolint:gosec // len(buf) is at most the 32767-character Windows path limit
		if err != nil {
			return "", &winPathError{op: "GetFinalPathNameByHandle", path: path, err: err}
		}
		if int(n) < len(buf) {
			return stripVerbatim(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n) // n includes the terminating NUL when too small
	}
}

// winPathError keeps the Windows error (for errors.Is(err, fs.ErrNotExist))
// while naming the operation.
type winPathError struct {
	op, path string
	err      error
}

func (e *winPathError) Error() string { return e.op + " " + e.path + ": " + e.err.Error() }
func (e *winPathError) Unwrap() error { return e.err }

// stripVerbatim turns the \\?\ form returned by GetFinalPathNameByHandle into
// a regular path: \\?\C:\x -> C:\x, \\?\UNC\srv\share\x -> \\srv\share\x.
func stripVerbatim(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`):
		return p[len(`\\?\`):]
	}
	return p
}

// nameEqual compares volume names and path components case-insensitively, as
// NTFS and SMB do by default. Unicode simple folding approximates the
// filesystem's upcase table.
func nameEqual(a, b string) bool { return strings.EqualFold(a, b) }
