//go:build !windows

package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWithinUnix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		root, path string
		want       bool
	}{
		{"/data", "/data", true},
		{"/data", "/data/x/y", true},
		{"/data", "/data2", false},
		{"/data", "/data2/x", false},
		{"/data", "/dat", false},
		{"/data", "/", false},
		{"/data", "/DATA/x", false}, // case-sensitive
		{"/", "/anything", true},
		{"/", "/", true},
		{"/a/b", "/a", false},
		{"/a/b", "/a/bc", false},
	}
	for _, tt := range tests {
		if got := within(tt.root, tt.path); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
		}
	}
}

func TestResolveUnixSpecialNames(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	p := mustPolicy(t, root)
	// Names that are special on Windows are ordinary on Unix.
	for _, name := range []string{"NUL", "a:b", "con.txt", " lead", "trail ", "-dash", "new\nline"} {
		path := filepath.Join(root, name)
		if got, err := p.Resolve(path); err != nil || got != path {
			t.Errorf("Resolve(%q) = (%q, %v)", path, got, err)
		}
	}
}

func TestResolveThroughFile(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := mustPolicy(t, root)
	if _, err := p.Resolve(filepath.Join(file, "child")); !errors.Is(err, ErrUnresolvable) {
		t.Errorf("path through a regular file: err = %v, want ErrUnresolvable", err)
	}
}

func TestResolveUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	t.Parallel()
	root := realTempDir(t)
	locked := filepath.Join(root, "locked")
	mkdirs(t, filepath.Join(locked, "inner"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	p := mustPolicy(t, root)
	if _, err := p.Resolve(filepath.Join(locked, "inner", "x")); !errors.Is(err, ErrUnresolvable) {
		t.Errorf("err = %v, want ErrUnresolvable", err)
	}
}
