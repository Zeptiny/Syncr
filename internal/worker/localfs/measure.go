package localfs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Measurement is a measured folder (Policy.Measure): its totals and each
// direct child folder's.
type Measurement struct {
	// Path is the measured folder, cleaned.
	Path string
	// Bytes is the size of every file below Path (links excluded); Files
	// and Dirs count the files (links included, as entries of their own)
	// and the folders below it.
	Bytes, Files, Dirs int64
	// Children are the direct child folders' totals, largest first; at
	// most the maxChildren Measure was given.
	Children []ChildMeasurement
	// ChildrenTruncated reports that more child folders were found than
	// returned.
	ChildrenTruncated bool
	// Complete reports that the walk covered everything. It is false when
	// ctx's deadline ended the walk early; every number is then a lower
	// bound.
	Complete bool
	// Unreadable counts the entries the walk could not read (permission
	// denied, vanished while walking) and skipped; links are never
	// followed, so their targets do not count. With any, the totals are
	// lower bounds even when Complete is true.
	Unreadable int64
}

// ChildMeasurement is one direct child folder's totals.
type ChildMeasurement struct {
	// Name is the child folder's base name.
	Name string
	// Bytes, Files and Dirs are as Measurement's, below the child.
	Bytes, Files, Dirs int64
}

// measureCheckEvery is how many entries the walk handles between checks of
// its context, besides one per batch of directory entries.
const measureCheckEvery = 256

// Measure walks path, which Resolve must accept, and sums what is below it
// per direct child folder. path itself is opened as List opens it (a link
// to a folder inside the roots is measured), but nothing below it is
// followed: symbolic links, and on Windows junctions, mount points and
// every other name-surrogate reparse point, are counted as entries and
// never entered, so the walk stays below path. (Go reports such entries
// as links or irregular files, never as folders.) Folders on other
// filesystems mounted below path are walked, except Linux pseudo
// filesystems (proc, sysfs, cgroup, debugfs and the like, told by their
// filesystem type): they hold kernel state, not data (/proc/kcore alone
// reports the address space's size), so such a folder counts as a folder
// and is not entered.
//
// An entry that cannot be read (a folder without permission, a file that
// vanished) is skipped and counted in Unreadable, not fatal. Hard links
// count once per name. Children are sorted largest first, then by name,
// and at most maxChildren are returned (maxChildren <= 0: none).
//
// Folders are read in batches and ctx is checked between batches and
// every few entries. When ctx's deadline passes the walk stops and Measure
// returns what it found with Complete false and no error; when ctx is
// cancelled it returns ctx's error. A system call blocked in the kernel (a
// hung network mount) cannot be interrupted from Go: Measure returns when
// that call does.
//
// Errors are ctx's, those of Resolve, the filesystem's for path itself
// (fs.ErrNotExist, fs.ErrPermission), or ErrInvalidPath when path is not a
// folder.
func (p *Policy) Measure(ctx context.Context, path string, maxChildren int) (Measurement, error) {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return Measurement{}, err
	}
	clean, err := p.Resolve(path)
	if err != nil {
		return Measurement{}, err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return Measurement{}, err
	}
	if !info.IsDir() {
		return Measurement{}, &PathError{Path: path, Err: fmt.Errorf("%w: not a folder", ErrInvalidPath)}
	}
	w := &walker{ctx: ctx, open: openDir, pseudo: pseudoFS}
	m, err := w.measure(clean, maxChildren)
	m.Path = clean
	return m, err
}

// dirReader reads a folder's entries in batches (*os.File).
type dirReader interface {
	ReadDir(n int) ([]fs.DirEntry, error)
	Close() error
}

// openDir opens a folder for reading.
func openDir(dir string) (dirReader, error) {
	return os.Open(dir) //nolint:gosec // below a folder accepted by Resolve; links are never followed
}

// errStop ends a walk whose context ended.
var errStop = errors.New("walk stopped")

// walker sums a folder tree. open and pseudo are seams for tests (a
// folder that cannot be read, one on a pseudo filesystem).
type walker struct {
	ctx  context.Context
	open func(dir string) (dirReader, error)
	// pseudo reports an opened folder on a pseudo filesystem, which the
	// walk does not enter (nil: none is).
	pseudo func(dirReader) bool
	// seen counts entries since the last context check.
	seen       int
	unreadable int64
}

// totals are the sums below one folder.
type totals struct{ bytes, files, dirs int64 }

// measure walks root (a folder) and returns its measurement without Path.
func (w *walker) measure(root string, maxChildren int) (Measurement, error) {
	var m Measurement
	var own totals
	children := make([]ChildMeasurement, 0, min(max(maxChildren, 0), 1024))
	// keep trims children to the largest maxChildren once they are twice
	// as many, so memory stays bounded however many folders root holds.
	keep := func(force bool) {
		limit := max(maxChildren, 0)
		if len(children) > limit && (force || len(children) > 2*limit+1024) {
			sortChildren(children)
			clear(children[limit:])
			children, m.ChildrenTruncated = children[:limit], true
		}
	}
	err := w.readDir(root, true, func(de fs.DirEntry) error {
		if !de.IsDir() {
			w.file(de, &own)
			return nil
		}
		own.dirs++
		var sub totals
		err := w.tree(filepath.Join(root, de.Name()), &sub)
		children = append(children, ChildMeasurement{Name: de.Name(), Bytes: sub.bytes, Files: sub.files, Dirs: sub.dirs})
		own.bytes, own.files, own.dirs = own.bytes+sub.bytes, own.files+sub.files, own.dirs+sub.dirs
		keep(false)
		return err
	})
	sortChildren(children)
	keep(true)
	m.Bytes, m.Files, m.Dirs = own.bytes, own.files, own.dirs
	m.Children, m.Unreadable = children, w.unreadable
	switch {
	case err == nil:
		m.Complete = true
	case errors.Is(err, errStop):
		if cerr := w.ctx.Err(); !errors.Is(cerr, context.DeadlineExceeded) {
			return Measurement{}, cerr
		}
	default:
		return Measurement{}, err
	}
	return m, nil
}

// tree adds everything below the folder dir to t, depth first without
// recursion.
func (w *walker) tree(dir string, t *totals) error {
	stack := []string{dir}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		err := w.readDir(cur, false, func(de fs.DirEntry) error {
			if de.IsDir() {
				t.dirs++
				stack = append(stack, filepath.Join(cur, de.Name()))
				return nil
			}
			w.file(de, t)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// file counts an entry that is not a folder. A link counts as an entry
// but adds no bytes; any other entry adds its size (on Windows a cloud
// placeholder, which Go reports as irregular, has its logical size).
func (w *walker) file(de fs.DirEntry, t *totals) {
	t.files++
	if de.Type()&fs.ModeSymlink != 0 {
		return
	}
	info, err := de.Info()
	if err != nil {
		w.unreadable++ // vanished since the folder was read
		return
	}
	if !info.IsDir() {
		t.bytes += max(info.Size(), 0)
	}
}

// readDir calls fn for each entry of dir, read in batches, and checks the
// context between batches and every measureCheckEvery entries (errStop
// once it ended). A folder that cannot be opened or read is counted as
// unreadable and skipped, except the root (top), whose error is returned.
// An error of fn ends the walk.
func (w *walker) readDir(dir string, top bool, fn func(fs.DirEntry) error) error {
	if w.ctx.Err() != nil {
		return errStop
	}
	f, err := w.open(dir)
	if err != nil {
		if top {
			return err
		}
		w.unreadable++
		return nil
	}
	defer f.Close()
	if w.pseudo != nil && w.pseudo(f) {
		return nil
	}
	for {
		des, rerr := f.ReadDir(listBatch)
		for _, de := range des {
			if w.seen++; w.seen >= measureCheckEvery {
				w.seen = 0
				if w.ctx.Err() != nil {
					return errStop
				}
			}
			if err := fn(de); err != nil {
				return err
			}
		}
		switch {
		case errors.Is(rerr, io.EOF):
			return nil
		case rerr != nil:
			if top && len(des) == 0 {
				return rerr
			}
			w.unreadable++
			return nil
		}
		if w.ctx.Err() != nil {
			return errStop
		}
	}
}

// sortChildren orders child folders largest first, then by file count,
// then by name.
func sortChildren(children []ChildMeasurement) {
	slices.SortFunc(children, func(a, b ChildMeasurement) int {
		if c := cmp.Compare(b.Bytes, a.Bytes); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Files, a.Files); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}
