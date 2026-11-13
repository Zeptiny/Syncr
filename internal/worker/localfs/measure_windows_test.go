//go:build windows

package localfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMeasureNeverFollowsJunctions: junctions (mount-point reparse
// points, which need no privilege to create) count as entries and are
// never entered, whether they lead outside the roots or back into the
// walk. Go (1.23 and later) reports them as irregular files, never as
// folders, which is what the walk relies on.
func TestMeasureNeverFollowsJunctions(t *testing.T) {
	t.Parallel()
	p, root := measureFixture(t)
	outside := filepath.Join(filepath.Dir(root), "outside")
	junction(t, filepath.Join(root, "out"), outside)
	junction(t, filepath.Join(root, "docs", "loop"), root)

	des, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		if de.Name() == "loop" && (de.IsDir() || de.Type()&fs.ModeIrregular == 0) {
			t.Errorf("a junction is listed with type %v; the walk would enter it", de.Type())
		}
	}

	m, err := p.Measure(t.Context(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's 4011 bytes in 6 files and 5 folders, plus 2 entries.
	if m.Bytes != 4011 || m.Files != 8 || m.Dirs != 5 || !m.Complete || m.Unreadable != 0 {
		t.Errorf("measurement = %+v", m)
	}
	want := []ChildMeasurement{
		{Name: "photos", Bytes: 4000, Files: 2, Dirs: 1},
		{Name: "docs", Bytes: 6, Files: 3, Dirs: 1},
		{Name: "empty"},
	}
	if !slices.Equal(m.Children, want) {
		t.Errorf("children = %+v, want %+v", m.Children, want)
	}

	// A symbolic link to a folder is not entered either (creating one
	// needs Developer Mode or admin rights).
	symlink(t, outside, filepath.Join(root, "photos", "link"))
	if m, err := p.Measure(t.Context(), root, 10); err != nil || m.Bytes != 4011 || m.Files != 9 {
		t.Errorf("with a directory symlink: %+v, %v", m, err)
	}
}
