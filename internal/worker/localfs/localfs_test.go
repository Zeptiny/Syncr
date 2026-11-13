package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// realTempDir returns a fresh temp dir with symlinks resolved, so tests can
// compare returned paths literally (macOS /var -> /private/var, Windows 8.3
// short names).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustPolicy(t *testing.T, roots ...string) *Policy {
	t.Helper()
	p, err := NewPolicy(roots)
	if err != nil {
		t.Fatalf("NewPolicy(%q): %v", roots, err)
	}
	return p
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlinks needs Developer Mode or admin rights on Windows: %v", err)
		}
		t.Fatal(err)
	}
}

func TestNewPolicyRejectsInvalidRoots(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"", "relative/dir", ".", "a\x00b"} {
		if _, err := NewPolicy([]string{root}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("NewPolicy(%q) err = %v, want ErrInvalidPath", root, err)
		}
	}
}

func TestPolicyRoots(t *testing.T) {
	t.Parallel()
	a, b := realTempDir(t), realTempDir(t)
	p := mustPolicy(t, a+string(filepath.Separator), filepath.Join(b, "x", ".."))
	if got := p.Roots(); !slices.Equal(got, []string{a, b}) {
		t.Errorf("Roots() = %q, want %q", got, []string{a, b})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root := filepath.Join(base, "data")
	sibling := filepath.Join(base, "data2")
	mkdirs(t, filepath.Join(root, "sub"), sibling)
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := mustPolicy(t, root)
	sep := string(filepath.Separator)

	tests := []struct {
		name string
		path string
		want string // cleaned result; empty when an error is expected
		err  error
	}{
		{"root itself", root, root, nil},
		{"root with trailing separator", root + sep, root, nil},
		{"existing dir", filepath.Join(root, "sub"), filepath.Join(root, "sub"), nil},
		{"existing file", filepath.Join(root, "file.txt"), filepath.Join(root, "file.txt"), nil},
		{"missing restore target", filepath.Join(root, "new", "deeper"), filepath.Join(root, "new", "deeper"), nil},
		{"dot-dot inside", root + sep + "sub" + sep + ".." + sep + "new", filepath.Join(root, "new"), nil},
		{"dot-dot escaping", root + sep + ".." + sep + "data2", "", ErrNotAllowed},
		{"sibling with common prefix", sibling, "", ErrNotAllowed},
		{"sibling child", filepath.Join(sibling, "x"), "", ErrNotAllowed},
		{"parent", base, "", ErrNotAllowed},
		{"relative", filepath.Join("data", "sub"), "", ErrInvalidPath},
		{"dot", ".", "", ErrInvalidPath},
		{"empty", "", "", ErrInvalidPath},
		{"NUL byte", root + sep + "a\x00b", "", ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.Resolve(tt.path)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Resolve(%q) = (%q, %v), want %v", tt.path, got, err, tt.err)
				}
				if _, ok := errors.AsType[*PathError](err); !ok {
					t.Errorf("Resolve(%q) error %T is not a *PathError", tt.path, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Resolve(%q) = (%q, %v), want %q", tt.path, got, err, tt.want)
			}
		})
	}
}

func TestResolveWithoutRootsDeniesEverything(t *testing.T) {
	t.Parallel()
	dir := realTempDir(t)
	for _, roots := range [][]string{nil, {}} {
		p := mustPolicy(t, roots...)
		if _, err := p.Resolve(dir); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("roots %q: Resolve() err = %v, want ErrNotAllowed", roots, err)
		}
	}
}

func TestResolveMultipleRoots(t *testing.T) {
	t.Parallel()
	a, b, c := realTempDir(t), realTempDir(t), realTempDir(t)
	p := mustPolicy(t, a, b)
	for _, path := range []string{filepath.Join(a, "x"), filepath.Join(b, "y", "z")} {
		if _, err := p.Resolve(path); err != nil {
			t.Errorf("Resolve(%q): %v", path, err)
		}
	}
	if _, err := p.Resolve(filepath.Join(c, "x")); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("path in a third dir: err = %v, want ErrNotAllowed", err)
	}
}

func TestResolveNonexistentRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(realTempDir(t), "later", "created")
	p := mustPolicy(t, root)
	if got, err := p.Resolve(filepath.Join(root, "x")); err != nil || got != filepath.Join(root, "x") {
		t.Errorf("Resolve() = (%q, %v)", got, err)
	}
	if _, err := p.Resolve(filepath.Dir(root)); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("parent of a missing root: err = %v, want ErrNotAllowed", err)
	}
}

func TestResolveSymlinks(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	mkdirs(t, filepath.Join(root, "real"), outside)

	symlink(t, outside, filepath.Join(root, "escape"))                         // inside -> outside
	symlink(t, filepath.Join(root, "real"), filepath.Join(base, "way-in"))     // outside -> inside
	symlink(t, filepath.Join(outside, "nothing"), filepath.Join(root, "dang")) // dangling, to outside
	symlink(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))      // inside -> inside
	p := mustPolicy(t, root)

	tests := []struct {
		name string
		path string
		err  error
	}{
		{"link escaping the root", filepath.Join(root, "escape"), ErrNotAllowed},
		{"child of an escaping link", filepath.Join(root, "escape", "new", "file"), ErrNotAllowed},
		{"link from outside into the root", filepath.Join(base, "way-in", "x"), nil},
		{"dangling link", filepath.Join(root, "dang"), ErrUnresolvable},
		{"child of a dangling link", filepath.Join(root, "dang", "x"), ErrUnresolvable},
		{"link within the root", filepath.Join(root, "alias", "x"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.Resolve(tt.path)
			if tt.err == nil {
				if err != nil || got != tt.path {
					t.Fatalf("Resolve(%q) = (%q, %v), want the cleaned path", tt.path, got, err)
				}
				return
			}
			if !errors.Is(err, tt.err) {
				t.Fatalf("Resolve(%q) = (%q, %v), want %v", tt.path, got, err, tt.err)
			}
		})
	}
}

func TestResolveSymlinkedRoot(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	realDir := filepath.Join(base, "real")
	mkdirs(t, realDir)
	link := filepath.Join(base, "link")
	symlink(t, realDir, link)
	p := mustPolicy(t, link)

	for _, path := range []string{filepath.Join(link, "x"), filepath.Join(realDir, "x")} {
		if got, err := p.Resolve(path); err != nil || got != path {
			t.Errorf("Resolve(%q) = (%q, %v)", path, got, err)
		}
	}
	if _, err := p.Resolve(base); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("Resolve(base) err = %v, want ErrNotAllowed", err)
	}
}

func TestOverlap(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	state := filepath.Join(base, "data", "bisync")
	mkdirs(t, filepath.Join(state, "key", "work"), filepath.Join(base, "shared"))

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same folder", state, state, true},
		{"inside", filepath.Join(state, "key", "work"), state, true},
		{"holds it", filepath.Join(base, "data"), state, true},
		{"holds it, the other way round", state, base, true},
		{"not there yet, inside", filepath.Join(state, "new", "dir"), state, true},
		{"beside it", filepath.Join(base, "shared"), state, false},
		{"a name that starts the same", filepath.Join(base, "data", "bisync2"), state, false},
		{"lexically inside", filepath.Join(base, "shared", "..", "data", "bisync", "x"), state, true},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct {
			name string
			a, b string
			want bool
		}{"another case", filepath.Join(base, "DATA", "BiSync", "key"), state, true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := Overlap(tt.a, tt.b); err != nil || got != tt.want {
				t.Errorf("Overlap(%q, %q) = %v, %v; want %v", tt.a, tt.b, got, err, tt.want)
			}
		})
	}

	t.Run("through a link", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(base, "shared", "into-state")
		symlink(t, filepath.Join(state, "key"), link)
		for _, p := range []string{link, filepath.Join(link, "work")} {
			if got, err := Overlap(p, state); err != nil || !got {
				t.Errorf("Overlap(%q) = %v, %v; want true", p, got, err)
			}
		}
	})

	for _, bad := range [][2]string{{"relative", state}, {state, ""}} {
		if _, err := Overlap(bad[0], bad[1]); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Overlap(%q, %q) err = %v, want ErrInvalidPath", bad[0], bad[1], err)
		}
	}
}

func TestResolveDotDotIsLexical(t *testing.T) {
	t.Parallel()
	// root/link/.. would be "outside/.." -> base if resolved physically; the
	// policy cleans first, so the path means root/x and is returned as such,
	// which is also what the OS opens.
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "elsewhere", "deep")
	mkdirs(t, root, outside)
	symlink(t, outside, filepath.Join(root, "link"))
	p := mustPolicy(t, root)

	sep := string(filepath.Separator)
	got, err := p.Resolve(root + sep + "link" + sep + ".." + sep + "x")
	if want := filepath.Join(root, "x"); err != nil || got != want {
		t.Errorf("Resolve() = (%q, %v), want %q", got, err, want)
	}
}

func TestPathErrorMessage(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, realTempDir(t))
	_, err := p.Resolve("relative")
	if err == nil || err.Error() != `local path "relative": not an absolute local path: relative` {
		t.Errorf("error = %v", err)
	}
}
