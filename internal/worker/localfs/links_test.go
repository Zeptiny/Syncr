package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeAreas runs WriteAreas and returns the folders it visited.
func writeAreas(t *testing.T, p *Policy, dir string) ([]string, error) {
	t.Helper()
	var visited []string
	err := p.WriteAreas(t.Context(), dir, func(folder string) error {
		visited = append(visited, folder)
		return nil
	})
	return visited, err
}

// TestWriteAreas: a folder rclone writes into may hold links to files
// anywhere and links to folders inside the roots, whose folders are
// visited and checked in turn; a link to a folder outside the roots, to a
// reserved folder or to one that holds a reserved folder fails the check
// with ErrLink, naming the link. This is the restore-then-copy escape: a
// restore recreates a link to /etc, and a copy into the same folder would
// write through it.
func TestWriteAreas(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root, other, outside := filepath.Join(base, "root"), filepath.Join(base, "other"), filepath.Join(base, "outside")
	data := filepath.Join(root, "data")
	dst, shared := filepath.Join(root, "dst"), filepath.Join(other, "shared")
	mkdirs(t, filepath.Join(dst, "sub"), shared, outside, filepath.Join(data, "tools"))
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := mustPolicy(t, root, other).Reserve(data)
	if err != nil {
		t.Fatal(err)
	}

	// Links to files and folders the jobs may use pass; the linked folder
	// is visited.
	symlink(t, filepath.Join(outside, "passwd"), filepath.Join(dst, "sub", "file-link"))
	symlink(t, shared, filepath.Join(dst, "sub", "shared-link"))
	symlink(t, "sub", filepath.Join(dst, "loop")) // inside the folder: visited once
	got, err := writeAreas(t, p, dst)
	if err != nil || !slices.Equal(got, []string{dst, shared}) {
		t.Fatalf("WriteAreas() = %q, %v", got, err)
	}

	for _, tt := range []struct {
		name, link, target string
	}{
		{"a folder outside the roots", filepath.Join(dst, "sub", "evil"), outside},
		{"the reserved folder", filepath.Join(dst, "tools"), filepath.Join(data, "tools")},
		{"a folder holding the reserved one", filepath.Join(dst, "up"), root},
		// The linked folder is checked too.
		{"through a linked folder", filepath.Join(shared, "evil"), outside},
	} {
		symlink(t, tt.target, tt.link)
		_, err := writeAreas(t, p, dst)
		var pe *PathError
		if !errors.Is(err, ErrLink) || !errors.Is(err, ErrNotAllowed) || !errors.As(err, &pe) || pe.Path != tt.link {
			t.Errorf("%s: WriteAreas() = %v, want ErrLink for %s", tt.name, err, tt.link)
		}
		if err := os.Remove(tt.link); err != nil {
			t.Fatal(err)
		}
	}

	// A reserved folder below the folder is not entered: what it holds is
	// the worker's business.
	symlink(t, outside, filepath.Join(data, "tools", "evil"))
	if got, err := writeAreas(t, p, root); err != nil || !slices.Contains(got, root) {
		t.Errorf("WriteAreas(root) with a link in the reserved folder = %q, %v", got, err)
	}

	// A folder that does not exist yet is visited, with nothing to check;
	// a file is not a folder.
	if got, err := writeAreas(t, p, filepath.Join(dst, "new", "dir")); err != nil || !slices.Equal(got, []string{filepath.Join(dst, "new", "dir")}) {
		t.Errorf("WriteAreas(new) = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dst, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAreas(t, p, filepath.Join(dst, "f")); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("WriteAreas(file) = %v, want ErrInvalidPath", err)
	}
	// Paths the policy refuses are refused.
	for _, dir := range []string{outside, filepath.Join(data, "tools")} {
		if _, err := writeAreas(t, p, dir); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("WriteAreas(%s) = %v, want ErrNotAllowed", dir, err)
		}
	}
}

// TestWriteAreasDangling: a link whose target does not exist cannot be
// written through yet, but would be once a tool creates the target, so its
// target must be a path the policy accepts, without "..".
func TestWriteAreasDangling(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	dst := filepath.Join(root, "dst")
	mkdirs(t, dst, outside)
	p := mustPolicy(t, root)
	for _, tt := range []struct {
		name, target string
		ok           bool
	}{
		{"absolute, inside the roots", filepath.Join(root, "later"), true},
		{"relative, below the link", filepath.Join("later", "x"), true},
		{"absolute, outside the roots", filepath.Join(outside, "later"), false},
		// "a" does not exist; once a copy creates dst/a, the link leads
		// two levels above it.
		{"with ..", filepath.Join("a", "..", "..", "..", "outside", "later"), false},
	} {
		link := filepath.Join(dst, "dangling")
		symlink(t, tt.target, link)
		_, err := writeAreas(t, p, dst)
		if tt.ok != (err == nil) || (!tt.ok && !errors.Is(err, ErrLink)) {
			t.Errorf("%s: WriteAreas() = %v", tt.name, err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWriteAreasVisitError: an error of visit ends the check and is
// returned as is, before the folder is read.
func TestWriteAreasVisitError(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	mkdirs(t, filepath.Join(root, "dst"))
	symlink(t, filepath.Join(root, "nowhere", "..", "..", "x"), filepath.Join(root, "dst", "bad"))
	busy := errors.New("busy")
	err := mustPolicy(t, root).WriteAreas(t.Context(), filepath.Join(root, "dst"), func(string) error { return busy })
	if err != busy { //nolint:errorlint // the very error
		t.Errorf("WriteAreas() = %v, want %v", err, busy)
	}
	if err := mustPolicy(t, root).WriteAreas(t.Context(), filepath.Join(root, "dst"), nil); !errors.Is(err, ErrLink) ||
		!strings.Contains(err.Error(), "bad") {
		t.Errorf("WriteAreas() without visit = %v", err)
	}
}
