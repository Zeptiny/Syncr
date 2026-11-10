package localfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	l, err := p.List(root, 0, false)
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
	l, err := p.List(root, 3, false)
	if err != nil || len(l.Entries) != 3 || !l.Truncated {
		t.Errorf("limited = %v truncated=%v, %v", names(l), l.Truncated, err)
	}
	l, err = p.List(root, 4, false)
	if err != nil || len(l.Entries) != 4 || l.Truncated {
		t.Errorf("exact limit = %v truncated=%v, %v", names(l), l.Truncated, err)
	}
	l, err = p.List(root, 0, true)
	if err != nil || len(l.Entries) != 2 || !l.Entries[0].Dir || !l.Entries[1].Dir {
		t.Errorf("dirs only = %v, %v", names(l), err)
	}
	// A path that is cleaned lists the same folder.
	l, err = p.List(filepath.Join(root, "b", "..", "b"), 0, false)
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
	l, err := p.List("", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "" || len(l.Entries) != 2 || l.Entries[0].Path != r1 || l.Entries[0].Name != r1 || l.Entries[1].Path != r2 || !l.Entries[0].Dir {
		t.Errorf("roots = %+v", l)
	}
	if l, _ := p.List("", 1, false); len(l.Entries) != 1 || !l.Truncated {
		t.Errorf("limited roots = %+v", l)
	}
	none, err := NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := none.List("", 0, false); err != nil || len(l.Entries) != 0 {
		t.Errorf("no roots = %+v, %v", l, err)
	}
}

func TestListErrors(t *testing.T) {
	t.Parallel()
	p, root, outside := listFixture(t)
	if _, err := p.List(outside, 0, false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("outside = %v, want ErrNotAllowed", err)
	}
	if _, err := p.List(filepath.Join(root, "..", "outside"), 0, false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("dot-dot escape = %v, want ErrNotAllowed", err)
	}
	if _, err := p.List("relative", 0, false); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("relative = %v, want ErrInvalidPath", err)
	}
	if _, err := p.List(filepath.Join(root, "nope"), 0, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v, want fs.ErrNotExist", err)
	}
	if _, err := p.List(filepath.Join(root, "a.txt"), 0, false); err == nil {
		t.Error("listing a file succeeded")
	}
}
