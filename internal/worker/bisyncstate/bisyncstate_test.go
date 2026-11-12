package bisyncstate

import (
	"context"
	"errors"
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

var testNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

const (
	keyA = "0000000000000000000000000000000000000000000000000000000000000a0a"
	keyB = "0000000000000000000000000000000000000000000000000000000000000b0b"
	keyC = "0000000000000000000000000000000000000000000000000000000000000c0c"
	keyD = "0000000000000000000000000000000000000000000000000000000000000d0d"
	keyE = "0000000000000000000000000000000000000000000000000000000000000e0e"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(Config{Root: filepath.Join(t.TempDir(), "bisync"), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func open(t *testing.T, s *Store, key string) *Pair {
	t.Helper()
	p, err := s.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
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

func TestValidKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		keyA:                            true,
		strings.Repeat("f", 64):         true,
		strings.Repeat("f", 63):         false,
		strings.Repeat("f", 65):         false,
		strings.Repeat("F", 64):         false,
		strings.Repeat("g", 64):         false,
		"../" + strings.Repeat("a", 61): false,
		"":                              false,
		strings.Repeat("a", 63) + "/":   false,
		strings.Repeat("a", 63) + "\\":  false,
	} {
		if got := ValidKey(key); got != want {
			t.Errorf("ValidKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestNewRejectsRelativeRoot(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"", "bisync", filepath.Join("data", "bisync")} {
		if _, err := New(Config{Root: root}); err == nil {
			t.Errorf("New(%q) succeeded", root)
		}
	}
}

func TestOpenLayout(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	p := open(t, s, keyA)

	dir := filepath.Join(s.Root(), keyA)
	if p.Dir() != dir || p.WorkDir() != filepath.Join(dir, "work") || p.FiltersFile() != filepath.Join(dir, "filters.txt") {
		t.Errorf("paths: dir %q, work %q, filters %q", p.Dir(), p.WorkDir(), p.FiltersFile())
	}
	for _, d := range []string{s.Root(), dir, p.WorkDir()} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v, want 0700", d, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); err != nil {
		t.Errorf("lock file: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, LastRunFileName))
	if err != nil || string(data) != "2026-11-12T12:00:00Z\n" {
		t.Errorf("last-run = %q, %v", data, err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close() = %v", err)
	}
}

func TestOpenRejectsInvalidKeyBeforeTouchingDisk(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, key := range []string{"", "..", "../" + keyA[3:], strings.ToUpper(keyA), keyA[1:], keyA + "0"} {
		if _, err := s.Open(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Open(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if _, err := os.Stat(s.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the root was created: %v", err)
	}
}

func TestOpenBusy(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	p := open(t, s, keyA)
	if _, err := s.Open(keyA); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Open() = %v, want ErrBusy", err)
	}
	other := open(t, s, keyB) // other pairs are independent
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	again := open(t, s, keyA)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFresh(t *testing.T) {
	t.Parallel()
	const (
		header = "# bisync listing v1 from 2026-11-12T20:52:35.936556785+0000\n"
		entry  = `-        7 - - 2026-11-12T20:52:35.871428015+0000 "f1.txt"` + "\n"
		ses    = "data_a..data_b"
	)
	tests := []struct {
		name  string
		files map[string]string
		dirs  []string
		fresh bool
	}{
		{"empty work dir", nil, nil, true},
		{"stale rclone lock only", map[string]string{ses + ".lck": "{}"}, nil, true},
		{"listings", map[string]string{ses + ".path1.lst": header + entry, ses + ".path2.lst": header + entry}, nil, false},
		{"one listing with entries", map[string]string{ses + ".path1.lst": header, ses + ".path2.lst": header + entry}, nil, false},
		{"listings without entries (two empty sides)", map[string]string{ses + ".path1.lst": header, ses + ".path2.lst": header + "\n"}, nil, true},
		{"empty listing files", map[string]string{ses + ".path1.lst": "", ses + ".path2.lst": ""}, nil, true},
		{"previous listings with entries", map[string]string{ses + ".path1.lst-old": header + entry}, nil, false},
		{"previous listings without entries", map[string]string{ses + ".path1.lst": header, ses + ".path1.lst-old": header}, nil, true},
		{"after a critical error", map[string]string{ses + ".path1.lst-err": header, ses + ".path2.lst-err": header}, nil, false},
		{"next listings only (first resync interrupted)", map[string]string{ses + ".path1.lst-new": header + entry, ses + ".path2.lst-new": header + entry}, nil, true},
		{"next listings beside the current ones", map[string]string{ses + ".path1.lst": header + entry, ses + ".path1.lst-new": header + entry}, nil, false},
		{"dry-run listings only", map[string]string{ses + ".path1.lst-dry": header + entry, ses + ".path2.lst-dry-old": header + entry, ses + ".path1.lst-dry-new": entry}, nil, true},
		{"unknown suffix", map[string]string{ses + ".path2.lst-queue": ""}, nil, false},
		{"session name containing the suffix", map[string]string{"x.path1.lst_y..z.path2.lst-dry": header + entry}, nil, true},
		{"entry after comments and blank lines", map[string]string{ses + ".path1.lst": header + "\n# more\n   \n" + entry}, nil, false},
		{"entry longer than the read buffer", map[string]string{ses + ".path1.lst": header + "- " + strings.Repeat("x", 10000) + "\n"}, nil, false},
		{"long comment, no entry", map[string]string{ses + ".path1.lst": "#" + strings.Repeat("x", 10000) + "\n" + header}, nil, true},
		{"entry without a final line break", map[string]string{ses + ".path1.lst": header + strings.TrimSuffix(entry, "\n")}, nil, false},
		{"a directory named like a listing", nil, []string{ses + ".path1.lst"}, true},
		{"other files", map[string]string{"notes.txt": "x", ses + ".path1": "x"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := open(t, newStore(t), keyA)
			for name, content := range tt.files {
				writeFile(t, filepath.Join(p.WorkDir(), name), content)
			}
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(p.WorkDir(), d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			fresh, err := p.Fresh()
			if err != nil || fresh != tt.fresh {
				t.Errorf("Fresh() = %v, %v; want %v", fresh, err, tt.fresh)
			}
		})
	}
}

func TestClearStaleLocks(t *testing.T) {
	t.Parallel()
	p := open(t, newStore(t), keyA)
	for _, name := range []string{"a..b.lck", "c..d.lck", "a..b.path1.lst", "a..b.path1.lck.txt"} {
		writeFile(t, filepath.Join(p.WorkDir(), name), "x")
	}
	n, err := p.ClearStaleLocks()
	if err != nil || n != 2 {
		t.Fatalf("ClearStaleLocks() = %d, %v; want 2", n, err)
	}
	entries, err := os.ReadDir(p.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"a..b.path1.lck.txt", "a..b.path1.lst"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
	if n, err := p.ClearStaleLocks(); n != 0 || err != nil {
		t.Errorf("second ClearStaleLocks() = %d, %v", n, err)
	}
}

func TestWriteFilters(t *testing.T) {
	t.Parallel()
	p := open(t, newStore(t), keyA)
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(p.FiltersFile())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if err := p.WriteFilters([]string{"- *.tmp", "+ /photos/**", "- -rf x"}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "- *.tmp\n+ /photos/**\n- -rf x\n" {
		t.Errorf("filters file = %q", got)
	}
	if err := p.WriteFilters(nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "" {
		t.Errorf("filters file without rules = %q, want empty", got)
	}
	for _, bad := range [][]string{{"- a\n+ b"}, {"- a\r"}, {"- a\x00"}, {""}} {
		if err := p.WriteFilters(bad); err == nil {
			t.Errorf("WriteFilters(%q) succeeded", bad)
		}
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	old := testNow.Add(-Retention - time.Hour)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) + "\n" }
	pair := func(key string) string {
		dir := filepath.Join(s.Root(), key)
		writeFile(t, filepath.Join(dir, WorkDirName, "s.path1.lst"), "# header\n")
		writeFile(t, filepath.Join(dir, FiltersFileName), "")
		return dir
	}

	stale := pair(keyA)
	writeFile(t, filepath.Join(stale, LastRunFileName), stamp(old))

	recent := pair(keyB)
	writeFile(t, filepath.Join(recent, LastRunFileName), stamp(testNow.Add(-Retention+time.Hour)))

	locked := pair(keyC)
	writeFile(t, filepath.Join(locked, LastRunFileName), stamp(old))
	release, ok, err := filelock.TryLock(filepath.Join(locked, LockFileName))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer func() { _ = release() }()

	// Without a readable last-run, the file's or the directory's
	// modification time counts.
	garbled := pair(keyD)
	writeFile(t, filepath.Join(garbled, LastRunFileName), "not a time")
	if err := os.Chtimes(filepath.Join(garbled, LastRunFileName), old, old); err != nil {
		t.Fatal(err)
	}
	missing := pair(keyE)
	if err := os.Chtimes(missing, old, old); err != nil {
		t.Fatal(err)
	}

	// Entries that are not pairs stay, whatever their age.
	notKey := filepath.Join(s.Root(), "not-a-key")
	writeFile(t, filepath.Join(notKey, LastRunFileName), stamp(old))
	fileKey := filepath.Join(s.Root(), strings.Repeat("f", 64))
	writeFile(t, fileKey, "x")

	removed, err := s.Prune(t.Context(), Retention)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{keyA, keyD, keyE}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for dir, kept := range map[string]bool{stale: false, garbled: false, missing: false, recent: true, locked: true, notKey: true, fileKey: true} {
		if _, err := os.Stat(dir); (err == nil) != kept {
			t.Errorf("%s: kept = %v, want %v", filepath.Base(dir), err == nil, kept)
		}
	}

	// A pruned pair opens as a fresh one.
	p := open(t, s, keyA)
	if fresh, err := p.Fresh(); !fresh || err != nil {
		t.Errorf("Fresh() after pruning = %v, %v", fresh, err)
	}
}

func TestPruneWithoutRoot(t *testing.T) {
	t.Parallel()
	removed, err := newStore(t).Prune(t.Context(), Retention)
	if removed != nil || err != nil {
		t.Errorf("Prune() = %v, %v", removed, err)
	}
}

func TestPruneStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeFile(t, filepath.Join(s.Root(), keyA, LastRunFileName), "2000-01-01T00:00:00Z\n")
	ctx, cancel := context.WithCancelCause(t.Context())
	stop := errors.New("stopping")
	cancel(stop)
	removed, err := s.Prune(ctx, Retention)
	if len(removed) != 0 || !errors.Is(err, stop) {
		t.Errorf("Prune() = %v, %v; want nothing removed and the cause", removed, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), keyA)); err != nil {
		t.Errorf("pair removed: %v", err)
	}
}

func TestOpenWaitsForPrune(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	dir := filepath.Join(s.Root(), keyA)
	writeFile(t, filepath.Join(dir, LastRunFileName), "2000-01-01T00:00:00Z\n")
	writeFile(t, filepath.Join(dir, WorkDirName, "s.path1.lst"), "- entry\n")

	// Prune holds mu while it removes a pair: an Open of that pair either
	// finds it whole and stale-free (before) or recreated (after).
	s.mu.Lock()
	opened := make(chan error, 1)
	go func() {
		p, err := s.Open(keyA)
		if err == nil {
			err = p.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("Open() returned while mu was held: %v", err)
	default:
	}
	s.mu.Unlock()
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	// The Open recorded a run, so the pair is no longer stale.
	if removed, err := s.Prune(t.Context(), Retention); len(removed) != 0 || err != nil {
		t.Errorf("Prune() after a run = %v, %v", removed, err)
	}
}

func TestPruneLoop(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	stale := func(key string) {
		writeFile(t, filepath.Join(s.Root(), key, LastRunFileName), "2000-01-01T00:00:00Z\n")
	}

	type pass struct {
		removed []string
		err     error
	}
	passes := make(chan pass)
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pruneLoop(ctx, ticks, func(removed []string, err error) { passes <- pass{removed, err} })
	}()

	// Nothing happens before a tick; each tick is one pass.
	stale(keyA)
	ticks <- testNow
	if p := <-passes; !slices.Equal(p.removed, []string{keyA}) || p.err != nil {
		t.Errorf("first pass = %+v, want %s removed", p, keyA)
	}
	stale(keyB)
	ticks <- testNow
	if p := <-passes; !slices.Equal(p.removed, []string{keyB}) || p.err != nil {
		t.Errorf("second pass = %+v, want %s removed", p, keyB)
	}
	ticks <- testNow
	if p := <-passes; len(p.removed) != 0 || p.err != nil {
		t.Errorf("idle pass = %+v", p)
	}
	cancel()
	<-done
}

func TestPruneStaleLogs(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeFile(t, filepath.Join(s.Root(), keyA, LastRunFileName), "2000-01-01T00:00:00Z\n")
	var logs strings.Builder
	s.PruneStale(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)))
	if _, err := os.Stat(filepath.Join(s.Root(), keyA)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale pair kept: %v", err)
	}
	if !strings.Contains(logs.String(), "pairs=1") {
		t.Errorf("logs = %q", logs.String())
	}
	s.PruneStale(t.Context(), nil) // nothing left, nothing logged, no panic
}
