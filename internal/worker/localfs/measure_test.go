package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// measureFixture is a tree below root: 4011 bytes in 6 files and 5
// folders, and a policy on root.
func measureFixture(t *testing.T) (p *Policy, root string) {
	t.Helper()
	base := realTempDir(t)
	root = filepath.Join(base, "root")
	mkfile(t, filepath.Join(root, "a.txt"), "hello")
	mkfile(t, filepath.Join(root, ".hidden"), "")
	mkfile(t, filepath.Join(root, "photos", "2025", "img1.jpg"), strings.Repeat("j", 3000))
	mkfile(t, filepath.Join(root, "photos", "x.jpg"), strings.Repeat("x", 1000))
	mkfile(t, filepath.Join(root, "docs", "readme.md"), "read")
	mkfile(t, filepath.Join(root, "docs", "deep", "e.txt"), "ee")
	mkdirs(t, filepath.Join(root, "empty"))
	mkfile(t, filepath.Join(base, "outside", "big.bin"), strings.Repeat("o", 9999))
	return mustPolicy(t, root), root
}

func childNames(m Measurement) []string {
	out := make([]string, len(m.Children))
	for i, c := range m.Children {
		out[i] = c.Name
	}
	return out
}

func TestMeasure(t *testing.T) {
	t.Parallel()
	p, root := measureFixture(t)
	m, err := p.Measure(t.Context(), root+string(filepath.Separator)+".", 10)
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != root || m.Bytes != 4011 || m.Files != 6 || m.Dirs != 5 || !m.Complete || m.Unreadable != 0 || m.ChildrenTruncated {
		t.Errorf("measurement = %+v", m)
	}
	// Largest first; an empty folder is a child too.
	want := []ChildMeasurement{
		{Name: "photos", Bytes: 4000, Files: 2, Dirs: 1},
		{Name: "docs", Bytes: 6, Files: 2, Dirs: 1},
		{Name: "empty"},
	}
	if !slices.Equal(m.Children, want) {
		t.Errorf("children = %+v, want %+v", m.Children, want)
	}

	// A sub-folder, and a bound on the children.
	m, err = p.Measure(t.Context(), filepath.Join(root, "photos"), 5)
	if err != nil || m.Bytes != 4000 || m.Files != 2 || m.Dirs != 1 || !slices.Equal(childNames(m), []string{"2025"}) {
		t.Errorf("photos = %+v, %v", m, err)
	}
	for _, limit := range []int{1, 0, -1} {
		m, err = p.Measure(t.Context(), root, limit)
		if err != nil || len(m.Children) != max(limit, 0) || !m.ChildrenTruncated || m.Bytes != 4011 {
			t.Errorf("limit %d: %+v, %v", limit, m, err)
		}
	}
	if m, _ := p.Measure(t.Context(), root, 1); m.Children[0].Name != "photos" {
		t.Errorf("the largest child was not kept: %+v", m.Children)
	}
}

func TestMeasureErrors(t *testing.T) {
	t.Parallel()
	p, root := measureFixture(t)
	outside := filepath.Join(filepath.Dir(root), "outside")
	for _, tc := range []struct {
		path string
		want error
	}{
		{outside, ErrNotAllowed},
		{"", ErrInvalidPath},
		{"relative", ErrInvalidPath},
		{filepath.Join(root, "a.txt"), ErrInvalidPath}, // not a folder
		{filepath.Join(root, "missing"), fs.ErrNotExist},
	} {
		if m, err := p.Measure(t.Context(), tc.path, 5); !errors.Is(err, tc.want) || m.Path != "" {
			t.Errorf("Measure(%q) = %+v, %v; want %v", tc.path, m, err, tc.want)
		}
	}
}

// failingDir is a dirReader that returns entries, then err.
type failingDir struct {
	entries []fs.DirEntry
	err     error
}

func (d *failingDir) ReadDir(int) ([]fs.DirEntry, error) {
	des := d.entries
	d.entries = nil
	if des == nil {
		return nil, d.err
	}
	return des, nil
}

func (d *failingDir) Close() error { return nil }

func TestMeasureSkipsUnreadableEntries(t *testing.T) {
	t.Parallel()
	_, root := measureFixture(t)
	deep := filepath.Join(root, "docs", "deep")
	photos := filepath.Join(root, "photos")
	w := &walker{ctx: t.Context(), open: func(dir string) (dirReader, error) {
		switch dir {
		case deep:
			return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
		case photos:
			// Fails after its first batch: what was read counts.
			f, err := os.Open(dir)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			des, err := f.ReadDir(-1)
			if err != nil {
				return nil, err
			}
			slices.SortFunc(des, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			return &failingDir{entries: des[:1], err: errors.New("i/o error")}, nil // "2025" only
		}
		return openDir(dir)
	}}
	m, err := w.measure(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Not counted: e.txt (2 bytes, its folder unreadable) and x.jpg (1000
	// bytes, after the read error); the folders themselves are.
	if m.Bytes != 3009 || m.Files != 4 || m.Dirs != 5 || m.Unreadable != 2 || !m.Complete {
		t.Errorf("measurement = %+v", m)
	}

	// The measured folder itself must be readable.
	w = &walker{ctx: t.Context(), open: func(string) (dirReader, error) { return nil, fs.ErrPermission }}
	if _, err := w.measure(root, 10); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("unreadable root = %v", err)
	}
	w = &walker{ctx: t.Context(), open: func(string) (dirReader, error) { return &failingDir{err: errors.New("i/o error")}, nil }}
	if _, err := w.measure(root, 10); err == nil {
		t.Error("a root that cannot be read was measured")
	}
}

// endAfter is a context whose Err reports err from its n-th call on: the
// walk checks its context only through Err.
type endAfter struct {
	context.Context
	err   error
	mu    sync.Mutex
	calls int
	n     int
}

func (c *endAfter) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.n {
		return c.err
	}
	return nil
}

func TestMeasureEndsWithItsContext(t *testing.T) {
	t.Parallel()
	p, root := bigFolder(t, 30, 2000)
	for i := range 30 {
		mkfile(t, filepath.Join(root, fmt.Sprintf("d-%04d", i), "f"), "1234")
	}
	full, err := p.Measure(t.Context(), root, 100)
	if err != nil || full.Files != 2030 || full.Dirs != 30 || full.Bytes != 120 || !full.Complete {
		t.Fatalf("full measurement = %+v, %v", full, err)
	}

	// The deadline (the executor's soft deadline): what was found, with
	// Complete false and no error.
	for _, n := range []int{1, 2, 5, 12} {
		ctx := &endAfter{Context: t.Context(), err: context.DeadlineExceeded, n: n}
		m, err := p.Measure(ctx, root, 100)
		if err != nil || m.Complete || m.Path != root || m.Files >= full.Files || m.Bytes > full.Bytes {
			t.Errorf("deadline after %d checks: %+v, %v", n, m, err)
		}
	}
	// A cancellation is an error.
	for _, n := range []int{1, 5} {
		ctx := &endAfter{Context: t.Context(), err: context.Canceled, n: n}
		if _, err := p.Measure(ctx, root, 100); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled after %d checks: %v", n, err)
		}
	}
}

func TestMeasureBoundsTheChildrenItHolds(t *testing.T) {
	t.Parallel()
	p, root := bigFolder(t, 1500, 0)
	mkfile(t, filepath.Join(root, "d-0700", "f"), "big")
	m, err := p.Measure(t.Context(), root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(childNames(m), []string{"d-0700", "d-0000"}) || !m.ChildrenTruncated || m.Dirs != 1500 || m.Files != 1 {
		t.Errorf("measurement: children %v, truncated %v, %d dirs, %d files", childNames(m), m.ChildrenTruncated, m.Dirs, m.Files)
	}
}

func TestSortChildren(t *testing.T) {
	t.Parallel()
	c := []ChildMeasurement{{Name: "b", Bytes: 1}, {Name: "a", Bytes: 1}, {Name: "z", Bytes: 9}, {Name: "y", Bytes: 1, Files: 3}}
	sortChildren(c)
	var got []string
	for _, x := range c {
		got = append(got, x.Name)
	}
	if want := []string{"z", "y", "a", "b"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Compile-time check: *os.File is a dirReader.
var _ dirReader = (*os.File)(nil)
