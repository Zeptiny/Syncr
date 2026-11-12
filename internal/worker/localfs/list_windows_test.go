//go:build windows

package localfs

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestListWindowsPaths(t *testing.T) {
	t.Parallel()
	p, root, _ := listFixture(t)
	// Roots and paths compare case-insensitively; the listing keeps the
	// spelling that was asked for.
	upper := strings.ToUpper(root)
	l, err := p.List(t.Context(), upper, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(l), []string{"b", "Z"}) || l.Path != upper {
		t.Errorf("listing %q = %v", l.Path, names(l))
	}
	if vol := filepath.VolumeName(l.Entries[0].Path); vol == "" {
		t.Errorf("entry path %q has no drive letter", l.Entries[0].Path)
	}
	for _, bad := range []string{`C:relative`, `\rooted`, `\\.\C:\`, `\\?\C:\x`, filepath.Join(root, "NUL")} {
		if _, err := p.List(t.Context(), bad, 0, false); err == nil {
			t.Errorf("List(%q) succeeded", bad)
		}
	}
	// A UNC path outside the roots is refused, not listed.
	if _, err := p.List(t.Context(), `\\server\share\folder`, 0, false); err == nil {
		t.Error("UNC path outside the roots was listed")
	}
}
