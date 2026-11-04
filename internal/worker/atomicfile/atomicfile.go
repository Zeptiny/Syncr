// Package atomicfile writes files that other processes must never see half
// written, with permissions suited to secrets.
//
// Write creates a temporary file next to the destination, restricts it,
// writes and syncs the data, and renames it into place. On Windows, where
// permission bits mean little, restricted files get an ACL limited to SYSTEM
// and Administrators (plus the current user on request); this is best effort
// and reported through Result rather than failing the write.
//
// Allowed dependencies: the standard library and golang.org/x/sys.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Options controls the permissions of the written file.
type Options struct {
	// Perm is the file mode on Unix. Zero means 0600.
	Perm fs.FileMode
	// RestrictACL limits the file to SYSTEM and Administrators on Windows.
	// It has no effect elsewhere (use Perm). Set it for files with secrets.
	RestrictACL bool
	// IncludeCurrentUser also grants the current user access when
	// RestrictACL is set, for per-user installs that run without admin
	// rights.
	IncludeCurrentUser bool
}

// Result reports non-fatal outcomes of Write.
type Result struct {
	// ACLWarning is set when the Windows ACL could not be restricted. The
	// file was still written, with the permissions it inherited.
	ACLWarning error
}

// Write atomically replaces path with data. The directory must exist.
func Write(path string, data []byte, opts Options) (res Result, err error) {
	perm := opts.Perm
	if perm == 0 {
		perm = 0o600
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	// Windows cannot represent most modes; the ACL below protects the file
	// there.
	if cerr := tmp.Chmod(perm); cerr != nil && !isWindows() {
		return Result{}, fmt.Errorf("set permissions of %s: %w", path, cerr)
	}
	if opts.RestrictACL {
		res.ACLWarning = restrictACL(tmpName, opts.IncludeCurrentUser)
	}
	if _, err = tmp.Write(data); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("sync %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("close %s: %w", path, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return Result{}, fmt.Errorf("replace %s: %w", path, err)
	}
	syncDir(dir)
	return res, nil
}
