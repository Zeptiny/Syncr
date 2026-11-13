//go:build integration

package rclone

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// selectionGolden is web/src/lib/rclone-filters.golden.json: a tree with
// awkward names, and selections the file browser turns into a source
// folder and filter lines, with what rclone matches for each.
type selectionGolden struct {
	Rclone string   `json:"rclone"`
	Tree   []string `json:"tree"`
	Cases  []struct {
		Name    string   `json:"name"`
		Source  string   `json:"source"`
		Filters []string `json:"filters"`
		Matched []string `json:"matched"`
	} `json:"cases"`
}

// validHere reports whether every name of a slash-separated path can
// exist on this platform. Windows refuses < > : " | ? * in names (and
// "\" is its separator), and a name that ends in a space or a dot.
func validHere(p string) bool {
	if runtime.GOOS != "windows" {
		return true
	}
	for name := range strings.SplitSeq(strings.TrimSuffix(p, "/"), "/") {
		if strings.ContainsAny(name, `<>:"|?*\`) || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
			return false
		}
	}
	return true
}

// TestIntegrationSelectionFilters copies and moves, with the real rclone
// on this platform's filesystem, the golden selections of the file
// browser: exactly what rclone matched when the golden file was made
// arrives, under the selected items' own names. This is what proves the
// filter escaping on a Windows worker ("\" before [ ] { } in names, which
// Windows allows): there the tree leaves out the names Windows cannot
// hold (with * ? \ or a trailing space), and what rclone matched of them.
func TestIntegrationSelectionFilters(t *testing.T) {
	it := newITest(t)
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "web", "src", "lib", "rclone-filters.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g selectionGolden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if g.Rclone != "v1.75.2" || len(g.Cases) == 0 {
		t.Fatalf("the golden file is for rclone %s with %d cases", g.Rclone, len(g.Cases))
	}
	build := func(root string) {
		for _, p := range g.Tree {
			if !validHere(p) {
				continue
			}
			full := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(p, "/")))
			if strings.HasSuffix(p, "/") {
				if err := os.MkdirAll(full, 0o700); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(p), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The file browser's options: the filters, empty folders created,
	// Keep existing, and for a move the emptied folders removed.
	options := func(lines []string, move bool) *syncrv1.RcloneTransferOptions {
		o := &syncrv1.RcloneTransferOptions{IgnoreExisting: true, CreateEmptySrcDirs: true, DeleteEmptySrcDirs: move}
		for _, l := range lines {
			if rest, ok := strings.CutPrefix(l, "- "); ok {
				o.Filters = append(o.Filters, exclude(rest))
			} else {
				o.Filters = append(o.Filters, include(strings.TrimPrefix(l, "+ ")))
			}
		}
		return o
	}
	// expected is what a case brings: its matched files (with their
	// golden path as content) and folders, those this platform can hold.
	expected := func(source string, matched []string) (map[string][]byte, []string) {
		files := map[string][]byte{}
		var dirs []string
		for _, m := range matched {
			if !validHere(path.Join(source, m)) {
				continue
			}
			if strings.HasSuffix(m, "/") {
				dirs = append(dirs, strings.TrimSuffix(m, "/"))
			} else {
				files[m] = []byte(path.Join(source, m))
			}
		}
		slices.Sort(dirs)
		return files, dirs
	}
	at := func(root, rel string) string {
		if rel == "" {
			return root
		}
		return filepath.Join(root, filepath.FromSlash(rel))
	}

	src := it.dir(t, "tree")
	build(src)
	for _, cs := range g.Cases {
		dst := it.dir(t, "copy-"+strings.ReplaceAll(cs.Name, " ", "-"))
		if _, _, err := it.run(t.Context(), t, modeCopy, local(at(src, cs.Source)), local(dst), options(cs.Filters, false)); err != nil {
			t.Fatalf("copy %q: %v", cs.Name, err)
		}
		files, dirs := expected(cs.Source, cs.Matched)
		if got := tree(t, dst); !maps.EqualFunc(got, files, slices.Equal) {
			t.Errorf("copy %q: arrived %q, want %q", cs.Name, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(files)))
		}
		if got := foldersBelow(t, dst); !slices.Equal(got, dirs) {
			t.Errorf("copy %q: folders %q, want %q", cs.Name, got, dirs)
		}
	}

	// A move takes exactly the selected items away.
	moved := it.dir(t, "moved")
	build(moved)
	before := tree(t, moved)
	for i, cs := range g.Cases {
		if cs.Name != "awkward folder names" && cs.Name != "siblings, a folder and a file" {
			continue
		}
		dst := it.dir(t, "move-"+strconv.Itoa(i))
		if _, _, err := it.run(t.Context(), t, modeMove, local(at(moved, cs.Source)), local(dst), options(cs.Filters, true)); err != nil {
			t.Fatalf("move %q: %v", cs.Name, err)
		}
		files, _ := expected(cs.Source, cs.Matched)
		if got := tree(t, dst); !maps.EqualFunc(got, files, slices.Equal) {
			t.Errorf("move %q: arrived %q, want %q", cs.Name, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(files)))
		}
		for m := range files {
			delete(before, path.Join(cs.Source, m))
		}
	}
	if got := tree(t, moved); !maps.EqualFunc(got, before, slices.Equal) {
		t.Errorf("after the moves the source holds %q, want %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(before)))
	}
}

// foldersBelow lists the folders below dir, slash-separated and sorted.
func foldersBelow(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
