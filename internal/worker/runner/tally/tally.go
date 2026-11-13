// Package tally sums a recursive listing into the measurement of the
// listed folder (syncrv1.ProbeMeasurement): its totals and those of each
// direct child folder. The rclone and restic probers feed it the entries
// of "rclone lsjson -R" and "restic ls --recursive" in whatever order the
// tool prints them.
//
// Allowed dependencies: the standard library, the generated protobuf code
// and internal/worker/runner (for ValidText).
package tally

import (
	"cmp"
	"slices"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// MaxTracked bounds the child folders a Tally keeps totals for, so a
// folder with millions of subfolders cannot exhaust the worker's memory.
// Entries below further children still count in the folder's totals, and
// the measurement says that children are missing (children_truncated).
const MaxTracked = 100_000

// Tally accumulates the entries below one folder. The zero value is not
// usable; call New. It is not safe for concurrent use.
type Tally struct {
	path        string
	maxChildren int
	bytes       int64
	files       int64
	dirs        int64
	unreadable  int64
	children    map[string]*syncrv1.ProbeChildMeasurement
	untracked   bool
}

// New returns a Tally for the folder path (reported as given) that keeps
// at most maxChildren child folders in its result; maxChildren <= 0 keeps
// none.
func New(path string, maxChildren int) *Tally {
	return &Tally{path: path, maxChildren: maxChildren, children: map[string]*syncrv1.ProbeChildMeasurement{}}
}

// Add counts one entry below the folder. rel is its path relative to the
// folder, slash-separated ("a/b/c.txt"; leading and trailing slashes are
// ignored, an empty rel is the folder itself and is not counted). dir
// reports a folder; size is a file's size in bytes, counted only for
// files and when positive (tools print -1 or 0 for unknown sizes).
func (t *Tally) Add(rel string, dir bool, size int64) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return
	}
	first, _, nested := strings.Cut(rel, "/")
	var c *syncrv1.ProbeChildMeasurement
	if nested || dir {
		c = t.child(first)
	}
	switch {
	case dir:
		t.dirs++
		if nested && c != nil {
			c.Dirs++
		}
	default:
		size = max(size, 0)
		t.files++
		t.bytes += size
		if c != nil {
			c.Files++
			c.Bytes += size
		}
	}
}

// child returns the totals of the child folder name, created on first
// sight; nil once MaxTracked children are tracked.
func (t *Tally) child(name string) *syncrv1.ProbeChildMeasurement {
	if c, ok := t.children[name]; ok {
		return c
	}
	if len(t.children) >= MaxTracked {
		t.untracked = true
		return nil
	}
	c := &syncrv1.ProbeChildMeasurement{Name: runner.ValidText(name)}
	t.children[name] = c
	return c
}

// Unreadable counts an entry the tool could not read.
func (t *Tally) Unreadable() { t.unreadable++ }

// Result returns the measurement. complete says the listing covered
// everything. Children come largest first (then by name), at most the
// maxChildren New was given.
func (t *Tally) Result(complete bool) *syncrv1.ProbeMeasurement {
	children := make([]*syncrv1.ProbeChildMeasurement, 0, len(t.children))
	for _, c := range t.children {
		children = append(children, c)
	}
	SortChildren(children)
	m := &syncrv1.ProbeMeasurement{
		Path:              runner.ValidText(t.path),
		Bytes:             t.bytes,
		Files:             t.files,
		Dirs:              t.dirs,
		Complete:          complete,
		Unreadable:        t.unreadable,
		ChildrenTruncated: t.untracked,
	}
	limit := max(t.maxChildren, 0)
	if len(children) > limit {
		clear(children[limit:])
		children, m.ChildrenTruncated = children[:limit], true
	}
	m.Children = children
	return m
}

// SortChildren orders child folders largest first, then by file count,
// then by name.
func SortChildren(children []*syncrv1.ProbeChildMeasurement) {
	slices.SortFunc(children, func(a, b *syncrv1.ProbeChildMeasurement) int {
		if c := cmp.Compare(b.GetBytes(), a.GetBytes()); c != 0 {
			return c
		}
		if c := cmp.Compare(b.GetFiles(), a.GetFiles()); c != 0 {
			return c
		}
		return cmp.Compare(a.GetName(), b.GetName())
	})
}
