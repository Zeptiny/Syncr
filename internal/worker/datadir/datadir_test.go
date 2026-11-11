package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLockRefusesASecondWorker(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data") // created by Lock
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("data directory: %v, %v", fi, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("data directory mode = %o, want 700", fi.Mode().Perm())
		}
	}

	_, err = Lock(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock() = %v, want ErrLocked", err)
	}
	for _, want := range []string{dir, "data_dir", "SYNCR_DATA_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	if err := release(); err != nil {
		t.Fatal(err)
	}
	release, err = Lock(dir)
	if err != nil {
		t.Fatalf("Lock() after release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestLockReportsAnUnusableDirectory(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(file); err == nil || errors.Is(err, ErrLocked) {
		t.Errorf("Lock(a file) = %v, want a plain error", err)
	}
}

func TestRemoveContents(t *testing.T) {
	t.Parallel()
	scratch := t.TempDir()
	rclone, restic := filepath.Join(scratch, "rclone"), filepath.Join(scratch, "restic")
	files := []string{
		filepath.Join(rclone, "rclone-job-1", "rclone.conf"),
		filepath.Join(rclone, "probe-1", "cache", "x"),
		filepath.Join(restic, "restic-job-2", "tmp-file"),
		filepath.Join(restic, "stray"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(scratch, "other", "keep")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemoveContents(rclone, restic, filepath.Join(scratch, "missing")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{rclone, restic} {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s: %d entries left, %v; want an empty directory", d, len(entries), err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a directory that was not named was touched: %v", err)
	}
}
