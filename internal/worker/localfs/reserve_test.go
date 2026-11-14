package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestReserve: a reserved folder and everything in it is refused with
// ErrReserved (and ErrNotAllowed) even inside a root, also through a
// symbolic link and before it exists; its parent and siblings are not.
func TestReserve(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	data := filepath.Join(root, "data")
	sources := filepath.Join(data, "sources")
	mkdirs(t, filepath.Join(data, "scratch"))
	base := mustPolicy(t, root)
	p, err := base.Reserve(sources)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Reserved(); !slices.Equal(got, []string{sources}) || len(base.Reserved()) != 0 {
		t.Errorf("Reserved() = %q (base %q)", got, base.Reserved())
	}

	refused := []string{sources, filepath.Join(sources, "mirror", "k", "data"), filepath.Join(sources, "..", "sources", "mount")}
	for _, path := range refused {
		_, err := p.Resolve(path)
		if !errors.Is(err, ErrReserved) || !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Resolve(%q) = %v, want ErrReserved", path, err)
		}
		if _, err := base.Resolve(path); err != nil {
			t.Errorf("the original policy changed: Resolve(%q) = %v", path, err)
		}
	}
	for _, path := range []string{root, data, filepath.Join(data, "scratch"), filepath.Join(data, "sources2"), filepath.Join(data, "source")} {
		if got, err := p.Resolve(path); err != nil || got != path {
			t.Errorf("Resolve(%q) = %q, %v", path, got, err)
		}
	}

	// Through a link inside a root that points into the reserved folder.
	mkdirs(t, filepath.Join(sources, "mirror"))
	link := filepath.Join(root, "link")
	symlink(t, filepath.Join(sources, "mirror"), link)
	if _, err := p.Resolve(filepath.Join(link, "k")); !errors.Is(err, ErrReserved) {
		t.Errorf("Resolve through a link = %v, want ErrReserved", err)
	}

	// Listings and measurements go through Resolve.
	if _, err := p.List(t.Context(), sources, 10, false); !errors.Is(err, ErrReserved) {
		t.Errorf("List(sources) = %v", err)
	}
	if _, err := p.Measure(t.Context(), filepath.Join(sources, "mirror"), 10); !errors.Is(err, ErrReserved) {
		t.Errorf("Measure(sources/mirror) = %v", err)
	}
	// A listing of the data directory still shows the folder's name.
	l, err := p.List(t.Context(), data, 10, false)
	if err != nil || !slices.ContainsFunc(l.Entries, func(e Entry) bool { return e.Name == "sources" }) {
		t.Errorf("List(data) = %+v, %v", l, err)
	}

	if _, err := base.Reserve("relative/dir"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("Reserve(relative) = %v", err)
	}
}

// TestMeasureSkipsReservedFolders: a measurement of a folder that holds a
// reserved folder counts it as an empty folder and never opens it, by its
// path as written or as resolved.
func TestMeasureSkipsReservedFolders(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	data := filepath.Join(base, "data")
	sources := filepath.Join(data, "sources")
	mkdirs(t, filepath.Join(sources, "mirror", "k", "data"), filepath.Join(data, "cache"))
	if err := os.WriteFile(filepath.Join(sources, "mirror", "k", "data", "big.bin"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "cache", "c.bin"), make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := mustPolicy(t, base).Reserve(sources)
	if err != nil {
		t.Fatal(err)
	}
	measure := func(path string) Measurement {
		t.Helper()
		m, err := p.Measure(t.Context(), path, 10)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := measure(data)
	if m.Bytes != 7 || m.Files != 1 || m.Dirs != 2 || !m.Complete {
		t.Errorf("measurement = %+v", m)
	}
	if i := slices.IndexFunc(m.Children, func(c ChildMeasurement) bool { return c.Name == "sources" }); i < 0 || m.Children[i] != (ChildMeasurement{Name: "sources"}) {
		t.Errorf("children = %+v", m.Children)
	}

	// The same through a link to the data directory: the walk compares the
	// resolved path too.
	link := filepath.Join(base, "data-link")
	symlink(t, data, link)
	if m := measure(link); m.Bytes != 7 {
		t.Errorf("measurement through a link = %+v", m)
	}
	// And the walker never opens the reserved folder.
	start, err := resolve(data)
	if err != nil {
		t.Fatal(err)
	}
	var opened []string
	w := &walker{ctx: t.Context(), open: func(dir string) (dirReader, error) {
		opened = append(opened, dir)
		return openDir(dir)
	}, skip: func(dir string) bool { return p.isReserved(data, start, dir) }}
	if _, err := w.measure(data, 10); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(opened, func(dir string) bool { return dir == sources || filepath.Dir(dir) == sources }) {
		t.Errorf("the walk opened the reserved folder: %q", opened)
	}
}
