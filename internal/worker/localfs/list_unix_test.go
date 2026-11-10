//go:build !windows

package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListSymlinks(t *testing.T) {
	t.Parallel()
	p, root, outside := listFixture(t)
	for link, target := range map[string]string{
		"in":       filepath.Join(root, "b"),
		"out":      outside,
		"dangling": filepath.Join(root, "gone"),
		"file":     filepath.Join(root, "a.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	l, err := p.List(root, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Links to folders sort with the folders.
	if want := []string{"Z", "b", "in", "out", ".hidden", "a.txt", "dangling", "file"}; !slices.Equal(names(l), want) {
		t.Fatalf("listing = %v, want %v", names(l), want)
	}
	for _, e := range l.Entries {
		wantLink := slices.Contains([]string{"in", "out", "dangling", "file"}, e.Name)
		if e.Symlink != wantLink {
			t.Errorf("%s: Symlink = %v", e.Name, e.Symlink)
		}
	}
	// A link is followed only into a root.
	if in, err := p.List(filepath.Join(root, "in"), 0, false); err != nil || !slices.Equal(names(in), []string{"inner.txt"}) {
		t.Errorf("link inside the roots = %v, %v", names(in), err)
	}
	if _, err := p.List(filepath.Join(root, "out"), 0, false); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("link outside the roots = %v, want ErrNotAllowed", err)
	}
	if dirs, _ := p.List(root, 0, true); !slices.Equal(names(dirs), []string{"Z", "b", "in", "out"}) {
		t.Errorf("dirs only = %v", names(dirs))
	}
}

func TestListUnreadableFolder(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	p, root, _ := listFixture(t)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := p.List(locked, 0, false); !errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v, want a permission error", err)
	}
}
