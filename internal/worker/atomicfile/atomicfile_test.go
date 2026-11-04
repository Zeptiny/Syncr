package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secret.yaml")
	for _, content := range []string{"first", "second, longer than the first"} {
		if _, err := Write(path, []byte(content), Options{RestrictACL: true}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("read = %q, %v; want %q", got, err, content)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %v, want 0600", perm)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the file (temporary files must not linger)", len(entries))
	}
}

func TestWriteHonoursPerm(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if _, err := Write(path, []byte("x"), Options{Perm: 0o644}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}
}

func TestWriteMissingDirectory(t *testing.T) {
	t.Parallel()
	_, err := Write(filepath.Join(t.TempDir(), "missing", "f"), []byte("x"), Options{})
	if err == nil {
		t.Fatal("want an error when the directory does not exist")
	}
}

func TestWriteKeepsOldFileOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if _, err := Write(path, []byte("old"), Options{}); err != nil {
		t.Fatal(err)
	}
	// Renaming a file over a directory fails on every platform.
	target := filepath.Join(dir, "d")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(target, []byte("new"), Options{}); err == nil {
		t.Fatal("want an error when the destination is a directory")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Errorf("unrelated file changed: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("directory has %d entries after a failed write, want 2 (temporary file removed)", len(entries))
	}
}
