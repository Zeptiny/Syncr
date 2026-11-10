package localfs

import (
	"cmp"
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

// List lists the direct children of path, which Resolve must accept; an
// empty path lists the roots themselves. Directories come first, then names
// in order (case-insensitively on Windows); at most limit entries are
// returned (limit <= 0 means no bound). With dirsOnly, only directories and
// links to directories are listed. Hidden files are included.
//
// Errors are those of Resolve, or the filesystem's (fs.ErrNotExist,
// fs.ErrPermission, a path that is not a directory).
func (p *Policy) List(path string, limit int, dirsOnly bool) (Listing, error) {
	if path == "" {
		return p.listRoots(limit), nil
	}
	clean, err := p.Resolve(path)
	if err != nil {
		return Listing{}, err
	}
	des, err := os.ReadDir(clean)
	if err != nil {
		return Listing{}, err
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		e, ok := entryOf(clean, de)
		if !ok || (dirsOnly && !e.Dir) {
			continue
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	out := Listing{Path: clean, Entries: entries}
	if limit > 0 && len(entries) > limit {
		out.Entries, out.Truncated = entries[:limit], true
	}
	return out, nil
}

// entryOf describes one directory entry; false if it vanished meanwhile.
func entryOf(dir string, de fs.DirEntry) (Entry, bool) {
	info, err := de.Info() // Lstat
	if err != nil {
		return Entry{}, false
	}
	e := Entry{
		Name:    de.Name(),
		Path:    filepath.Join(dir, de.Name()),
		Dir:     info.IsDir(),
		Symlink: info.Mode()&fs.ModeSymlink != 0,
		ModTime: info.ModTime(),
		Mode:    info.Mode(),
	}
	if info.Mode().IsRegular() {
		e.Size = info.Size()
	}
	if e.Symlink {
		// A dangling link, or one that cannot be followed, is not a folder.
		if target, err := os.Stat(e.Path); err == nil {
			e.Dir = target.IsDir()
		}
	}
	return e, true
}

func (p *Policy) listRoots(limit int) Listing {
	out := Listing{Entries: make([]Entry, 0, len(p.roots))}
	for _, r := range p.roots {
		e := Entry{Name: r.clean, Path: r.clean, Dir: true}
		if info, err := os.Stat(r.clean); err == nil {
			e.Dir, e.ModTime, e.Mode = info.IsDir(), info.ModTime(), info.Mode()
		}
		out.Entries = append(out.Entries, e)
	}
	sortEntries(out.Entries)
	if limit > 0 && len(out.Entries) > limit {
		out.Entries, out.Truncated = out.Entries[:limit], true
	}
	return out
}

// sortEntries orders directories first, then by name.
func sortEntries(entries []Entry) {
	key := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		key = strings.ToLower
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(key(a.Name), key(b.Name)), cmp.Compare(a.Name, b.Name))
	})
}
