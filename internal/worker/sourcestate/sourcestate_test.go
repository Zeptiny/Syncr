package sourcestate

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
)

var testNow = time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)

const (
	keyA = "0123456789abcdef0123456789abcdef"
	keyB = "11111111111111111111111111111111"
	keyC = "22222222222222222222222222222222"
	keyD = "33333333333333333333333333333333"
	keyE = "44444444444444444444444444444444"
)

const retention = 30 * 24 * time.Hour

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(Config{Root: filepath.Join(t.TempDir(), "sources"), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewRejectsRelativeRoot(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Root: "sources"}); err == nil {
		t.Error("New accepted a relative root")
	}
}

// TestOpenMirrorLayout: opening a mirror creates its folder (and the
// root's) with mode 0700, the marker file, the lock and last-run; closing
// records the time again and releases the lock.
func TestOpenMirrorLayout(t *testing.T) {
	t.Parallel()
	now := testNow
	s, err := New(Config{Root: filepath.Join(t.TempDir(), "sources"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.OpenMirror(keyA)
	if err != nil {
		t.Fatal(err)
	}
	if m.Dir() != filepath.Join(s.Root(), MirrorsDirName, keyA) || m.DataDir() != filepath.Join(m.Dir(), DataDirName) {
		t.Errorf("Dir = %q, DataDir = %q", m.Dir(), m.DataDir())
	}
	if runtime.GOOS != "windows" {
		for _, dir := range []string{s.Root(), s.MirrorsDir(), m.Dir()} {
			if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("%s: %v, %v", dir, info.Mode(), err)
			}
		}
	}
	if got := readFile(t, filepath.Join(s.Root(), MarkerName)); !strings.Contains(got, "Syncr worker") {
		t.Errorf("marker = %q", got)
	}
	if got := readFile(t, filepath.Join(m.Dir(), LastRunFileName)); got != "2026-11-14T03:00:00Z\n" {
		t.Errorf("last-run at open = %q", got)
	}
	if _, err := os.Stat(m.DataDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the copy exists before rclone made it: %v", err)
	}
	now = testNow.Add(time.Hour)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if got := readFile(t, filepath.Join(m.Dir(), LastRunFileName)); got != "2026-11-14T04:00:00Z\n" {
		t.Errorf("last-run at close = %q", got)
	}
	// The lock is free again.
	m2, err := s.OpenMirror(keyA)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = m2.Close()
}

// TestKeysAreCheckedBeforeTouchingDisk: a key that is not 32 lower-case hex
// digits never becomes a folder.
func TestKeysAreCheckedBeforeTouchingDisk(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, key := range []string{"", "../../../etc", strings.ToUpper(keyA), keyA + "0", keyA[:31], strings.Repeat("0", 64)} {
		if _, err := s.OpenMirror(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("OpenMirror(%q) = %v", key, err)
		}
		if _, err := s.LockMount(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("LockMount(%q) = %v", key, err)
		}
	}
	if _, err := os.Stat(s.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the root was created: %v", err)
	}
}

func TestOpenMirrorBusy(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	m, err := s.OpenMirror(keyA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := s.OpenMirror(keyA); !errors.Is(err, ErrBusy) {
		t.Errorf("second OpenMirror = %v, want ErrBusy", err)
	}
	other, err := s.OpenMirror(keyB)
	if err != nil {
		t.Fatalf("another key: %v", err)
	}
	_ = other.Close()
}

// TestLockMount: the lock of a mount point lies next to it; the point
// itself is neither created nor touched.
func TestLockMount(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	l, err := s.LockMount(keyA)
	if err != nil {
		t.Fatal(err)
	}
	if l.Point() != filepath.Join(s.MountsDir(), keyA) {
		t.Errorf("Point = %q", l.Point())
	}
	if _, err := os.Stat(l.Point()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the mount point was created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.MountsDir(), keyA+MountLockSuffix)); err != nil {
		t.Errorf("lock file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), MarkerName)); err != nil {
		t.Errorf("marker: %v", err)
	}
	if _, err := s.LockMount(keyA); !errors.Is(err, ErrBusy) {
		t.Errorf("second LockMount = %v, want ErrBusy", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := s.LockMount(keyA)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	_ = l2.Close()
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	old := testNow.Add(-retention - time.Hour)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) + "\n" }
	mirror := func(key string) string {
		dir := filepath.Join(s.MirrorsDir(), key)
		writeFile(t, filepath.Join(dir, DataDirName, "photo.jpg"), "x")
		return dir
	}

	stale := mirror(keyA)
	writeFile(t, filepath.Join(stale, LastRunFileName), stamp(old))

	recent := mirror(keyB)
	writeFile(t, filepath.Join(recent, LastRunFileName), stamp(testNow.Add(-retention+time.Hour)))

	locked := mirror(keyC)
	writeFile(t, filepath.Join(locked, LastRunFileName), stamp(old))
	release, ok, err := filelock.TryLock(filepath.Join(locked, LockFileName))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer func() { _ = release() }()

	// Without a readable last-run, the file's or the folder's modification
	// time counts.
	garbled := mirror(keyD)
	writeFile(t, filepath.Join(garbled, LastRunFileName), "not a time")
	if err := os.Chtimes(filepath.Join(garbled, LastRunFileName), old, old); err != nil {
		t.Fatal(err)
	}
	missing := mirror(keyE)
	if err := os.Chtimes(missing, old, old); err != nil {
		t.Fatal(err)
	}

	// Entries that are not mirrors stay, whatever their age, and nothing
	// outside mirror/ is looked at.
	notKey := filepath.Join(s.MirrorsDir(), "not-a-key")
	writeFile(t, filepath.Join(notKey, LastRunFileName), stamp(old))
	fileKey := filepath.Join(s.MirrorsDir(), strings.Repeat("f", 32))
	writeFile(t, fileKey, "x")
	mountPoint := filepath.Join(s.MountsDir(), keyA)
	writeFile(t, filepath.Join(mountPoint, LastRunFileName), stamp(old))

	removed, err := s.Prune(t.Context(), retention)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{keyA, keyD, keyE}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for dir, kept := range map[string]bool{
		stale: false, garbled: false, missing: false, recent: true, locked: true, notKey: true, fileKey: true, mountPoint: true,
	} {
		if _, err := os.Stat(dir); (err == nil) != kept {
			t.Errorf("%s: kept = %v, want %v", filepath.Base(dir), err == nil, kept)
		}
	}
}

func TestPruneWithoutRoot(t *testing.T) {
	t.Parallel()
	removed, err := newStore(t).Prune(t.Context(), retention)
	if err != nil || len(removed) != 0 {
		t.Errorf("Prune = %v, %v", removed, err)
	}
}

func TestPruneStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeFile(t, filepath.Join(s.MirrorsDir(), keyA, LastRunFileName), "2020-01-01T00:00:00Z\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	removed, err := s.Prune(ctx, retention)
	if !errors.Is(err, context.Canceled) || len(removed) != 0 {
		t.Errorf("Prune = %v, %v", removed, err)
	}
}

// TestPruneLoop prunes on every tick until its context ends, with the
// configured retention.
func TestPruneLoop(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	passes := make(chan []string)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pruneLoop(ctx, ticks, time.Hour, func(removed []string, err error) {
			if err != nil {
				t.Error(err)
			}
			passes <- removed
		})
	}()
	writeFile(t, filepath.Join(s.MirrorsDir(), keyA, LastRunFileName), testNow.Add(-2*time.Hour).Format(time.RFC3339Nano))
	ticks <- testNow
	if got := <-passes; !slices.Equal(got, []string{keyA}) {
		t.Errorf("first pass removed %v", got)
	}
	ticks <- testNow
	if got := <-passes; len(got) != 0 {
		t.Errorf("second pass removed %v", got)
	}
	cancel()
	<-done
}

func TestPruneStaleLogs(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeFile(t, filepath.Join(s.MirrorsDir(), keyA, LastRunFileName), "2020-01-01T00:00:00Z\n")
	var buf bytes.Buffer
	s.PruneStale(t.Context(), retention, slog.New(slog.NewTextHandler(&buf, nil)))
	if !strings.Contains(buf.String(), "mirrors=1") || !strings.Contains(buf.String(), "retention=720h0m0s") {
		t.Errorf("log = %s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(s.MirrorsDir(), keyA)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the mirror was kept: %v", err)
	}
}
