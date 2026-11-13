//go:build !windows

package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// TestMeasureNeverFollowsLinks: links count as entries without bytes and
// are never entered, whether they point inside the walk (a loop), outside
// the roots, nowhere, or to a file.
func TestMeasureNeverFollowsLinks(t *testing.T) {
	t.Parallel()
	p, root := measureFixture(t)
	outside := filepath.Join(filepath.Dir(root), "outside")
	for link, target := range map[string]string{
		"out":                 outside,
		"docs/loop":           "..",
		"docs/deep/self":      ".",
		"dangling":            filepath.Join(root, "gone"),
		"photos/2025/to-file": filepath.Join(root, "a.txt"),
	} {
		symlink(t, target, filepath.Join(root, link))
	}
	if err := syscall.Mkfifo(filepath.Join(root, "docs", "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := p.Measure(t.Context(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's 4011 bytes in 6 files and 5 folders, plus 6 entries.
	if m.Bytes != 4011 || m.Files != 12 || m.Dirs != 5 || !m.Complete || m.Unreadable != 0 {
		t.Errorf("measurement = %+v", m)
	}
	want := []ChildMeasurement{
		{Name: "photos", Bytes: 4000, Files: 3, Dirs: 1},
		{Name: "docs", Bytes: 6, Files: 5, Dirs: 1},
		{Name: "empty"},
	}
	if !slices.Equal(m.Children, want) {
		t.Errorf("children = %+v, want %+v", m.Children, want)
	}

	// The measured path itself may be a link to a folder inside the roots,
	// as for List; one that leads outside is refused.
	symlink(t, filepath.Join(root, "photos"), filepath.Join(root, "docs", "to-photos"))
	if m, err := p.Measure(t.Context(), filepath.Join(root, "docs", "to-photos"), 10); err != nil || m.Bytes != 4000 {
		t.Errorf("measuring through a link = %+v, %v", m, err)
	}
	if _, err := p.Measure(t.Context(), filepath.Join(root, "out"), 10); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("measuring a link out of the roots = %v, want ErrNotAllowed", err)
	}
}

func TestMeasureUnreadableFolder(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	p, root := measureFixture(t)
	locked := filepath.Join(root, "docs", "deep")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	m, err := p.Measure(t.Context(), root, 10)
	if err != nil || m.Unreadable != 1 || m.Bytes != 4009 || m.Files != 5 || m.Dirs != 5 || !m.Complete {
		t.Errorf("measurement = %+v, %v", m, err)
	}
	// The measured folder itself must be readable.
	if _, err := p.Measure(t.Context(), locked, 10); !errors.Is(err, os.ErrPermission) {
		t.Errorf("unreadable folder = %v, want a permission error", err)
	}
}
