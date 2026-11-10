package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
)

func names(l Listing) []string {
	out := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		out[i] = e.Name
	}
	return out
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// listFixture returns a policy on a root holding folders "b" and "Z",
// files "a.txt" (5 bytes) and ".hidden", and an outside folder.
func listFixture(t *testing.T) (p *Policy, root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	mkfile(t, filepath.Join(root, "a.txt"), "hello")
	mkfile(t, filepath.Join(root, ".hidden"), "")
	mkfile(t, filepath.Join(root, "b", "inner.txt"), "x")
	mkfile(t, filepath.Join(root, "Z", "inner.txt"), "x")
	mkfile(t, filepath.Join(outside, "secret.txt"), "s")
	p, err = NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return p, root, outside
}

func TestListOrdersDirectoriesFirst(t *testing.T) {
	t.Parallel()
	p, root, _ := listFixture(t)
	l, err := p.List(t.Context(), root, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Z", "b", ".hidden", "a.txt"}
	if runtime.GOOS == "windows" {
		want = []string{"b", "Z", ".hidden", "a.txt"} // case-insensitive
	}
	if !slices.Equal(names(l), want) || l.Path != root || l.Truncated {
		t.Errorf("listing %q = %v (truncated %v), want %v", l.Path, names(l), l.Truncated, want)
	}
	file := l.Entries[3]
	if file.Path != filepath.Join(root, "a.txt") || file.Size != 5 || file.Dir || file.Symlink || file.ModTime.IsZero() {
		t.Errorf("file entry = %+v", file)
	}
	if dir := l.Entries[0]; !dir.Dir || dir.Size != 0 {
		t.Errorf("dir entry = %+v", dir)
	}
}

func TestListLimitsAndFilters(t *testing.T) {
	t.Parallel()
	p, root, _ := listFixture(t)
	l, err := p.List(t.Context(), root, 3, false)
	if err != nil || len(l.Entries) != 3 || !l.Truncated {
		t.Errorf("limited = %v truncated=%v, %v", names(l), l.Truncated, err)
	}
	l, err = p.List(t.Context(), root, 4, false)
	if err != nil || len(l.Entries) != 4 || l.Truncated {
		t.Errorf("exact limit = %v truncated=%v, %v", names(l), l.Truncated, err)
	}
	l, err = p.List(t.Context(), root, 0, true)
	if err != nil || len(l.Entries) != 2 || !l.Entries[0].Dir || !l.Entries[1].Dir {
		t.Errorf("dirs only = %v, %v", names(l), err)
	}
	// A path that is cleaned lists the same folder.
	l, err = p.List(t.Context(), filepath.Join(root, "b", "..", "b"), 0, false)
	if err != nil || l.Path != filepath.Join(root, "b") || !slices.Equal(names(l), []string{"inner.txt"}) {
		t.Errorf("cleaned path = %q %v, %v", l.Path, names(l), err)
	}
}

func TestListEmptyPathListsTheRoots(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r1, r2 := filepath.Join(base, "one"), filepath.Join(base, "two")
	if err := os.MkdirAll(r1, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := NewPolicy([]string{r2, r1}) // r2 does not exist yet
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.List(t.Context(), "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "" || len(l.Entries) != 2 || l.Entries[0].Path != r1 || l.Entries[0].Name != r1 || l.Entries[1].Path != r2 || !l.Entries[0].Dir {
		t.Errorf("roots = %+v", l)
	}
	if l, _ := p.List(t.Context(), "", 1, false); len(l.Entries) != 1 || !l.Truncated {
		t.Errorf("limited roots = %+v", l)
	}
	none, err := NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := none.List(t.Context(), "", 0, false); err != nil || len(l.Entries) != 0 {
		t.Errorf("no roots = %+v, %v", l, err)
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	p, root, outside := listFixture(t)
	if _, err := p.List(t.Context(), outside, 0, false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("outside = %v, want ErrNotAllowed", err)
	}
	if _, err := p.List(t.Context(), filepath.Join(root, "..", "outside"), 0, false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("dot-dot escape = %v, want ErrNotAllowed", err)
	}
	if _, err := p.List(t.Context(), "relative", 0, false); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("relative = %v, want ErrInvalidPath", err)
	}
	if _, err := p.List(t.Context(), filepath.Join(root, "nope"), 0, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v, want fs.ErrNotExist", err)
	}
	if _, err := p.List(t.Context(), filepath.Join(root, "a.txt"), 0, false); err == nil {
		t.Error("listing a file succeeded")
	}
}

// cancelAfter is a context whose Err reports cancellation from its n-th
// call on: List checks its context only through Err.
type cancelAfter struct {
	context.Context
	mu    sync.Mutex
	calls int
	n     int
}

func (c *cancelAfter) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.n {
		return context.Canceled
	}
	return nil
}

// bigFolder makes a folder holding dirs folders "d-0000"... and files
// files "f-0000"... (empty), and a policy on it.
func bigFolder(t *testing.T, dirs, files int) (*Policy, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range dirs {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d-%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f-%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return p, root
}

func TestListLargeFolder(t *testing.T) {
	t.Parallel()
	const dirs, files = 40, 3000 // several batches of listBatch
	p, root := bigFolder(t, dirs, files)

	all, err := p.List(t.Context(), root, 0, false)
	if err != nil || len(all.Entries) != dirs+files || all.Truncated {
		t.Fatalf("unbounded: %d entries, truncated %v, %v", len(all.Entries), all.Truncated, err)
	}
	want := names(all)
	if !slices.IsSortedFunc(all.Entries, func(a, b Entry) int { return compareNames(dirName{a.Name, a.Dir}, dirName{b.Name, b.Dir}) }) ||
		want[0] != "d-0000" || want[dirs] != "f-0000" {
		t.Fatalf("unbounded listing is not in order: %v...", want[:5])
	}
	// A bound keeps the first entries of the full order, whatever order the
	// folder is read in and however many names were dropped on the way.
	for _, limit := range []int{1, dirs + 10, 700, dirs + files - 1, dirs + files} {
		l, err := p.List(t.Context(), root, limit, false)
		if err != nil || !slices.Equal(names(l), want[:limit]) || l.Truncated != (limit < dirs+files) {
			t.Errorf("limit %d: %d entries (first %v), truncated %v, %v", limit, len(l.Entries), names(l)[:min(3, len(l.Entries))], l.Truncated, err)
		}
		if limit == dirs+10 && (l.Entries[dirs].Size != 0 || l.Entries[dirs].ModTime.IsZero() || !l.Entries[0].Dir) {
			t.Errorf("limit %d: entries not described: %+v %+v", limit, l.Entries[0], l.Entries[dirs])
		}
	}
	dirsOnly, err := p.List(t.Context(), root, 5, true)
	if err != nil || !slices.Equal(names(dirsOnly), want[:5]) || !dirsOnly.Truncated {
		t.Errorf("dirs only: %v, truncated %v, %v", names(dirsOnly), dirsOnly.Truncated, err)
	}
}

func TestListStopsWithItsContext(t *testing.T) {
	t.Parallel()
	p, root := bigFolder(t, 10, 2000)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, path := range []string{root, ""} {
		if l, err := p.List(ctx, path, 0, false); !errors.Is(err, context.Canceled) || len(l.Entries) != 0 {
			t.Errorf("List(%q) with an ended context = %d entries, %v", path, len(l.Entries), err)
		}
	}
	// Ended between batches: the rest of the folder is not read.
	mid := &cancelAfter{Context: t.Context(), n: 3}
	if _, err := p.List(mid, root, 0, false); !errors.Is(err, context.Canceled) {
		t.Errorf("ended while reading = %v", err)
	}
	if mid.calls != mid.n {
		t.Errorf("List checked its context %d times after it ended", mid.calls-mid.n)
	}
	// Ended later (while the entries are described): it stops at once too.
	late := &cancelAfter{Context: t.Context(), n: 2000/listBatch + 4}
	if _, err := p.List(late, root, 0, false); !errors.Is(err, context.Canceled) || late.calls != late.n {
		t.Errorf("ended while describing = %v after %d checks", err, late.calls)
	}
}
