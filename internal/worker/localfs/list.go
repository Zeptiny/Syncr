package localfs

import (
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Entry is one listed file or folder.
type Entry struct {
	// Name is the base name; for a root (Listing of ""), the root itself.
	Name string
	// Path is the full local path, to list or use next.
	Path string
	// Dir reports a directory, or a symbolic link to one.
	Dir bool
	// Symlink reports a symbolic link. Listing its Path goes through
	// Resolve again, so it works only when the target is inside a root.
	Symlink bool
	// Size is the size in bytes of a regular file.
	Size int64
	// ModTime is the modification time.
	ModTime time.Time
	// Mode is the entry's own mode (of the link for a symbolic link).
	Mode fs.FileMode
}

// Listing is one listed folder.
type Listing struct {
	// Path is the listed folder, cleaned; empty for the roots.
	Path string
	// Entries are its direct children: directories first, then by name.
	Entries []Entry
	// Truncated reports that more entries exist than were returned.
	Truncated bool
}

// listBatch is how many directory entries List reads at a time; it checks
// its context between batches.
const listBatch = 512

// List lists the direct children of path, which Resolve must accept; an
// empty path lists the roots themselves. Directories come first, then names
// in order (case-insensitively on Windows); at most limit entries are
// returned (limit <= 0 means no bound). With dirsOnly, only directories and
// links to directories are listed. Hidden files are included.
//
// The folder is read in batches. Ordering needs every name and whether it
// is a folder (the directory entry's type; a symbolic link's target is
// stat'ed), but only the entries returned are stat'ed for their size, time
// and mode, and at most about twice limit names are held at once. List
// checks ctx between batches and before each stat and returns ctx's error
// once it ends. A single system call blocked in the kernel (a hung network
// mount) cannot be interrupted from Go: List returns when that call does.
//
// Errors are ctx's, those of Resolve, or the filesystem's (fs.ErrNotExist,
// fs.ErrPermission, a path that is not a directory).
func (p *Policy) List(ctx context.Context, path string, limit int, dirsOnly bool) (Listing, error) {
	if err := ctx.Err(); err != nil {
		return Listing{}, err
	}
	if path == "" {
		return p.listRoots(ctx, limit)
	}
	clean, err := p.Resolve(path)
	if err != nil {
		return Listing{}, err
	}
	names, truncated, err := readNames(ctx, clean, limit, dirsOnly)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Path: clean, Entries: make([]Entry, 0, len(names)), Truncated: truncated}
	for _, n := range names {
		if limit > 0 && len(out.Entries) == limit {
			out.Truncated = true // more names remain
			break
		}
		if err := ctx.Err(); err != nil {
			return Listing{}, err
		}
		e, ok := describe(clean, n)
		if !ok || (dirsOnly && !e.Dir) {
			continue // vanished or changed since the folder was read
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// dirName is a directory entry as read, before it is stat'ed: what
// ordering needs.
type dirName struct {
	name string
	// dir reports a directory, or a symbolic link to one.
	dir bool
}

// readNames reads the folder dir in batches and returns its entries in
// listing order: all of them when limit <= 0, else at least the first
// limit (truncated reports that others were dropped on the way).
func readNames(ctx context.Context, dir string, limit int, dirsOnly bool) (names []dirName, truncated bool, err error) {
	f, err := os.Open(dir) //nolint:gosec // dir was accepted by Resolve: inside local_roots
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	for {
		des, rerr := f.ReadDir(listBatch)
		for _, de := range des {
			n := dirName{name: de.Name(), dir: de.IsDir()}
			if de.Type()&fs.ModeSymlink != 0 {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
				// A dangling link, or one that cannot be followed, is not a
				// folder.
				if target, err := os.Stat(filepath.Join(dir, n.name)); err == nil {
					n.dir = target.IsDir()
				}
			}
			if dirsOnly && !n.dir {
				continue
			}
			names = append(names, n)
		}
		switch {
		case errors.Is(rerr, io.EOF):
			slices.SortFunc(names, compareNames)
			return names, truncated, nil
		case rerr != nil:
			return nil, false, rerr
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if limit > 0 && len(names) > 2*limit {
			// Only the first limit can be listed: keep those.
			slices.SortFunc(names, compareNames)
			clear(names[limit:])
			names, truncated = names[:limit], true
		}
	}
}

// describe stats one entry of dir; false if it vanished meanwhile. n.dir
// (a symbolic link's target, followed when the folder was read) is kept
// for a link.
func describe(dir string, n dirName) (Entry, bool) {
	path := filepath.Join(dir, n.name)
	info, err := os.Lstat(path)
	if err != nil {
		return Entry{}, false
	}
	e := Entry{
		Name:    n.name,
		Path:    path,
		Dir:     info.IsDir(),
		Symlink: info.Mode()&fs.ModeSymlink != 0,
		ModTime: info.ModTime(),
		Mode:    info.Mode(),
	}
	if info.Mode().IsRegular() {
		e.Size = info.Size()
	}
	if e.Symlink {
		e.Dir = n.dir
	}
	return e, true
}

func (p *Policy) listRoots(ctx context.Context, limit int) (Listing, error) {
	out := Listing{Entries: make([]Entry, 0, len(p.roots))}
	for _, r := range p.roots {
		if err := ctx.Err(); err != nil {
			return Listing{}, err
		}
		e := Entry{Name: r.clean, Path: r.clean, Dir: true}
		if info, err := os.Stat(r.clean); err == nil {
			e.Dir, e.ModTime, e.Mode = info.IsDir(), info.ModTime(), info.Mode()
		}
		out.Entries = append(out.Entries, e)
	}
	slices.SortFunc(out.Entries, func(a, b Entry) int {
		return compareNames(dirName{a.Name, a.Dir}, dirName{b.Name, b.Dir})
	})
	if limit > 0 && len(out.Entries) > limit {
		out.Entries, out.Truncated = out.Entries[:limit], true
	}
	return out, nil
}

// compareNames orders directories first, then by name (case-insensitively
// on Windows, ties broken by the exact name).
func compareNames(a, b dirName) int {
	if a.dir != b.dir {
		if a.dir {
			return -1
		}
		return 1
	}
	if runtime.GOOS == "windows" {
		if c := cmp.Compare(strings.ToLower(a.name), strings.ToLower(b.name)); c != 0 {
			return c
		}
	}
	return cmp.Compare(a.name, b.name)
}
