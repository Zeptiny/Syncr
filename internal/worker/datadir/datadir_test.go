package datadir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// noWaiting fails the test if Lock waits.
func noWaiting(t *testing.T) func(string) {
	return func(path string) { t.Errorf("Lock waited for %s", path) }
}

func TestLockRefusesASecondWorker(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data") // created by Lock
	release, err := Lock(t.Context(), dir, 0, noWaiting(t))
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

	_, err = Lock(t.Context(), dir, 0, noWaiting(t))
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
	release, err = Lock(t.Context(), dir, 0, noWaiting(t))
	if err != nil {
		t.Fatalf("Lock() after release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

// TestLockWaitsForAnExitingWorker: a worker started while the one it
// replaces still exits gets the lock once that one let it go.
func TestLockWaitsForAnExitingWorker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	previous, err := Lock(t.Context(), dir, 0, noWaiting(t))
	if err != nil {
		t.Fatal(err)
	}
	var waited []string
	// The previous worker exits as soon as the new one says it waits; the
	// wait is long enough never to end first.
	release, err := Lock(t.Context(), dir, time.Hour, func(path string) {
		waited = append(waited, path)
		if err := previous(); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatalf("Lock() = %v, want the lock once the previous holder released it", err)
	}
	defer func() { _ = release() }()
	if len(waited) != 1 || waited[0] != filepath.Join(dir, LockFileName) {
		t.Errorf("waiting called with %q, want the lock file once", waited)
	}
}

// TestLockStillRefusesAfterWaiting: a second worker that really runs keeps
// the lock, so the wait ends with the same refusal, saying it waited.
func TestLockStillRefusesAfterWaiting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other, err := Lock(t.Context(), dir, 0, noWaiting(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other() }()
	calls := 0
	_, err = Lock(t.Context(), dir, 100*time.Millisecond, func(string) { calls++ })
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Lock() = %v, want ErrLocked", err)
	}
	if calls != 1 {
		t.Errorf("waiting called %d times, want once", calls)
	}
	for _, want := range []string{dir, "still did after 100ms", "data_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestLockStopsWaitingWithItsContext: a stop request during the wait ends
// it with the context's error, not with ErrLocked.
func TestLockStopsWaitingWithItsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other, err := Lock(t.Context(), dir, 0, noWaiting(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other() }()
	ctx, cancel := context.WithCancelCause(t.Context())
	stopped := errors.New("stop requested")
	_, err = Lock(ctx, dir, time.Hour, func(string) { cancel(stopped) })
	if !errors.Is(err, stopped) || errors.Is(err, ErrLocked) {
		t.Fatalf("Lock() = %v, want the context's cause", err)
	}
}

func TestLockReportsAnUnusableDirectory(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(t.Context(), file, time.Hour, noWaiting(t)); err == nil || errors.Is(err, ErrLocked) {
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
