// Package bisyncstate keeps the state of two-way sync pairs (rclone bisync,
// the "rclone_bisync" job kind) on the worker, one directory per pair below
// a root (<data_dir>/bisync):
//
//	<root>/<state_key>/
//	    syncr.lock        Syncr's lock: only its holder runs rclone here
//	    work/             rclone's --workdir: the listings and rclone's .lck
//	    filters.txt       the pair's filter rules (rclone's --filters-file)
//	    filters.txt.md5   rclone's hash of them, written by a resync
//	    last-run          when a run last opened the pair, for retention
//
// A state key is 64 lower-case hex digits computed by the controller. The
// store checks only its format, before it becomes a directory name, and
// derives nothing from it.
//
// A run opens its pair (Store.Open), which takes syncr.lock without waiting
// and records the time in last-run. Holding the lock, it removes the .lck
// files a killed rclone left behind (Pair.ClearStaleLocks), decides whether
// the pair has a baseline to compare against (Pair.Fresh), writes the filter
// rules (Pair.WriteFilters) and runs rclone, then closes the pair. After a
// run that rclone stopped with a critical error, Pair.Resumable tells
// whether the next normal run can go on from the listings rclone kept. Prune
// removes pairs that have not run for Retention and whose lock is free; Open
// and Prune are serialized, so a pair is never removed while a run takes it.
//
// Allowed dependencies: the standard library and internal/worker/filelock.
package bisyncstate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
)

// Retention is how long the state of a pair that has not run is kept. A
// pruned pair is fresh again: its next run resyncs.
const Retention = 180 * 24 * time.Hour

// PruneInterval is how often a running worker prunes after its start-up
// pass (RunPruner).
const PruneInterval = 24 * time.Hour

// Names inside a pair's directory.
const (
	LockFileName    = "syncr.lock"
	WorkDirName     = "work"
	FiltersFileName = "filters.txt"
	LastRunFileName = "last-run"
)

// KeyLength is the length of a state key: a SHA-256 in lower-case hex.
const KeyLength = 64

// Sentinel errors, matched with errors.Is.
var (
	// ErrInvalidKey reports a state key that is not 64 lower-case hex
	// digits.
	ErrInvalidKey = errors.New("the state key must be 64 lower-case hex digits")
	// ErrBusy reports a pair whose lock another run holds.
	ErrBusy = errors.New("another run of this two-way sync is in progress on this worker")
)

// ValidKey reports whether key is a well-formed state key.
func ValidKey(key string) bool {
	if len(key) != KeyLength {
		return false
	}
	for i := range len(key) {
		if c := key[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Config configures a Store.
type Config struct {
	// Root is the absolute directory that holds the pairs' directories. It
	// is created (mode 0700) when a pair is first opened.
	Root string
	// Now tells the time; nil means time.Now.
	Now func() time.Time
}

// Store is the state of every pair below one root. It is safe for
// concurrent use; one Store must serve every user of the root in the
// process (the runner and the pruner), because it serializes Open with
// Prune.
type Store struct {
	root string
	now  func() time.Time
	// mu is held while a pair's lock is taken (Open) and while a pair is
	// removed (Prune), never while a run uses its pair.
	mu sync.Mutex
}

// New returns a Store for cfg. It touches nothing on disk.
func New(cfg Config) (*Store, error) {
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("bisyncstate: root %q is not absolute", cfg.Root)
	}
	s := &Store{root: filepath.Clean(cfg.Root), now: cfg.Now}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Root returns the directory that holds the pairs.
func (s *Store) Root() string { return s.root }

// Pair is an open pair: its lock is held until Close.
type Pair struct {
	dir     string
	release func() error
	once    sync.Once
	err     error
}

// Open takes the lock of the pair named key, without waiting, creating its
// directory (mode 0700) and work directory if needed, and records the time
// in last-run. It fails with ErrInvalidKey before key touches the file
// system, and with ErrBusy if another run holds the pair. The caller must
// Close the pair.
func (s *Store) Open(key string) (*Pair, error) {
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	dir := filepath.Join(s.root, key)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory: %w", err)
	}
	release, ok, err := filelock.TryLock(filepath.Join(dir, LockFileName))
	switch {
	case err != nil:
		return nil, fmt.Errorf("lock the state directory: %w", err)
	case !ok:
		return nil, ErrBusy
	}
	p := &Pair{dir: dir, release: release}
	if err := p.prepare(s.now()); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// prepare creates the work directory and records the run's start.
func (p *Pair) prepare(now time.Time) error {
	if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil {
		return fmt.Errorf("create the work directory: %w", err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(filepath.Join(p.dir, LastRunFileName), []byte(stamp), 0o600); err != nil {
		return fmt.Errorf("record the run in the state directory: %w", err)
	}
	return nil
}

// Close releases the pair's lock. Later calls return the first result.
func (p *Pair) Close() error {
	p.once.Do(func() { p.err = p.release() })
	return p.err
}

// Dir returns the pair's directory.
func (p *Pair) Dir() string { return p.dir }

// WorkDir returns rclone's work directory (--workdir).
func (p *Pair) WorkDir() string { return filepath.Join(p.dir, WorkDirName) }

// FiltersFile returns the filters file (--filters-file). rclone writes its
// hash next to it.
func (p *Pair) FiltersFile() string { return filepath.Join(p.dir, FiltersFileName) }

// ClearStaleLocks removes rclone's lock files (*.lck) from the work
// directory and returns how many it removed. Only the holder of the pair's
// lock runs rclone in it, so a .lck found there is from a run that was
// killed; rclone would refuse to run until it expired.
func (p *Pair) ClearStaleLocks() (int, error) {
	entries, err := os.ReadDir(p.WorkDir())
	if err != nil {
		return 0, fmt.Errorf("read the work directory: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".lck") {
			continue
		}
		if err := os.Remove(filepath.Join(p.WorkDir(), e.Name())); err != nil {
			return n, fmt.Errorf("remove a stale rclone lock: %w", err)
		}
		n++
	}
	return n, nil
}

// Fresh reports whether the pair has no baseline: nothing a normal run
// could compare against, so its run must be a resync. rclone names its
// listings "<session>.path1.lst" and "<session>.path2.lst", plus a suffix.
// A pair is fresh unless its work directory holds one of these:
//
//   - a listing ("" suffix) or a previous one ("-old") with at least one
//     entry. Listings without entries come from a resync of two empty
//     sides; rclone refuses to sync against them ("Empty prior PathN
//     listing"), so such a pair has no baseline either;
//   - a listing renamed after a critical error ("-err"), whatever it
//     holds: only a deliberate resync may follow a critical error;
//   - a listing with a suffix this package does not know.
//
// Dry-run listings ("-dry", "-dry-new", ...) never count: a dry run leaves
// the real state alone. Neither does a next listing alone ("-new"): a run
// always leaves the current listing behind once it completed, so "-new"
// without it is a first resync that was cancelled or killed, which rclone
// itself says to run again.
func (p *Pair) Fresh() (bool, error) {
	entries, err := os.ReadDir(p.WorkDir())
	if err != nil {
		return false, fmt.Errorf("read the work directory: %w", err)
	}
	for _, e := range entries {
		suffix, ok := listingSuffix(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		switch {
		case strings.HasPrefix(suffix, "-dry"), suffix == "-new":
			continue
		case suffix == "", suffix == "-old":
			has, err := hasEntries(filepath.Join(p.WorkDir(), e.Name()))
			if err != nil {
				return false, fmt.Errorf("read a listing: %w", err)
			}
			if has {
				return false, nil
			}
		default:
			return false, nil
		}
	}
	return true, nil
}

// listingSuffix returns what follows ".path1.lst" or ".path2.lst" in name,
// and whether name is a listing at all. The session part of the name comes
// from the two paths, so the last occurrence is the one rclone added.
func listingSuffix(name string) (string, bool) {
	i := max(strings.LastIndex(name, ".path1.lst"), strings.LastIndex(name, ".path2.lst"))
	if i < 0 {
		return "", false
	}
	return name[i+len(".path1.lst"):], true
}

// Resumable reports whether a normal run can go on from the pair's
// listings, without a resync, the way rclone looks for them: both current
// listings of a session ("<session>.path1.lst" and "<session>.path2.lst"),
// or, when one of them is missing, both previous ones ("-old"). rclone
// keeps the previous listings, the state before the last good run, when a
// critical error renames the current ones ("-err"), and its --recover
// falls back to them. The listings it would use must each have an entry:
// rclone refuses to sync against an empty one ("Empty prior PathN
// listing").
//
// It tells only what the listings allow. A run asks after rclone stopped
// with "Must run --resync"; changed filters need a resync whatever the
// listings are.
func (p *Pair) Resumable() (bool, error) {
	entries, err := os.ReadDir(p.WorkDir())
	if err != nil {
		return false, fmt.Errorf("read the work directory: %w", err)
	}
	// side is what the work directory holds of one side's listing.
	type side struct{ exists, entries bool }
	type session struct{ current, previous [2]side }
	sessions := map[string]*session{}
	for _, e := range entries {
		name, n, suffix, ok := parseListing(e.Name())
		if !ok || !e.Type().IsRegular() || (suffix != "" && suffix != "-old") {
			continue
		}
		has, err := hasEntries(filepath.Join(p.WorkDir(), e.Name()))
		if err != nil {
			return false, fmt.Errorf("read a listing: %w", err)
		}
		s := sessions[name]
		if s == nil {
			s = &session{}
			sessions[name] = s
		}
		l := &s.current
		if suffix == "-old" {
			l = &s.previous
		}
		l[n] = side{exists: true, entries: has}
	}
	for _, s := range sessions {
		use := s.current
		if !use[0].exists || !use[1].exists {
			use = s.previous
		}
		if use[0].entries && use[1].entries {
			return true, nil
		}
	}
	return false, nil
}

// parseListing splits the name of a listing into rclone's session, the
// side (0 for path1, 1 for path2) and the suffix after ".pathN.lst", or
// reports that name is not a listing.
func parseListing(name string) (session string, n int, suffix string, ok bool) {
	i1, i2 := strings.LastIndex(name, ".path1.lst"), strings.LastIndex(name, ".path2.lst")
	i := max(i1, i2)
	if i < 0 {
		return "", 0, "", false
	}
	if i == i2 {
		n = 1
	}
	return name[:i], n, name[i+len(".path1.lst"):], true
}

// hasEntries reports whether the listing at path has a line that is
// neither blank nor a "#" comment (its header).
func hasEntries(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // a file of the pair's work directory
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadSlice('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' {
			return true, nil
		}
		// The rest of a line longer than the buffer is skipped.
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = r.ReadSlice('\n')
		}
		switch {
		case errors.Is(err, io.EOF):
			return false, nil
		case err != nil:
			return false, err
		}
	}
}

// WriteFilters writes the filter rules, one per line ("+ pattern" or
// "- pattern", already checked by the caller), to the filters file. No
// rules give an empty file, which rclone still hashes, so removing every
// rule is noticed like any other change. The same rules always give the
// same bytes.
func (p *Pair) WriteFilters(rules []string) error {
	var b strings.Builder
	for i, r := range rules {
		if r == "" || strings.ContainsAny(r, "\r\n\x00") {
			return fmt.Errorf("filter rule %d is empty or contains a line break or NUL byte", i)
		}
		b.WriteString(r)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(p.FiltersFile(), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write the filters file: %w", err)
	}
	return nil
}

// Prune removes the directory of every pair whose last run is older than
// maxAge and whose lock is free, and returns the keys it removed. A pair
// without a readable last-run counts from the modification time of that
// file, or of its directory. Entries that are not pair directories are
// left alone. It stops early, with ctx's cause, when ctx ends.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the state root: %w", err)
	}
	cutoff := s.now().Add(-maxAge)
	var removed []string
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, context.Cause(ctx))
			break
		}
		if !e.IsDir() || !ValidKey(e.Name()) {
			continue
		}
		ok, err := s.pruneOne(e.Name(), cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("pair %s: %w", e.Name(), err))
		}
		if ok {
			removed = append(removed, e.Name())
		}
	}
	return removed, errors.Join(errs...)
}

// pruneOne removes the pair key if it last ran before cutoff and its lock
// is free, and reports whether it did.
func (s *Store) pruneOne(key string, cutoff time.Time) (bool, error) {
	dir := filepath.Join(s.root, key)
	if last, err := lastRun(dir); err != nil || !last.Before(cutoff) {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil // removed meanwhile
		}
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok, err := filelock.TryLock(filepath.Join(dir, LockFileName))
	if err != nil || !ok {
		return false, err // a run holds it, so it is not stale
	}
	// A run may have opened and closed the pair since the first look; it
	// recorded the time in last-run. (Taking the lock may have created the
	// lock file, so the directory's time no longer tells.)
	if ranSince(dir, cutoff) {
		return false, release()
	}
	// Everything but the lock file goes while the lock is held; Windows
	// does not delete a file that is open, so the lock file goes after the
	// release. Open waits on mu meanwhile.
	errContents := removeAllExcept(dir, LockFileName)
	if err := errors.Join(errContents, release()); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

// lastRun returns when the pair in dir last ran: the time recorded in
// last-run, else that file's modification time, else the directory's.
func lastRun(dir string) (time.Time, error) {
	path := filepath.Join(dir, LastRunFileName)
	data, err := os.ReadFile(path) //nolint:gosec // a file of the pair's directory
	if err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); perr == nil {
			return t, nil
		}
	}
	for _, p := range []string{path, dir} {
		info, err := os.Stat(p)
		if err == nil {
			return info.ModTime(), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, err
		}
	}
	return time.Time{}, fs.ErrNotExist
}

// ranSince reports whether the last-run file in dir records a run at or
// after cutoff.
func ranSince(dir string, cutoff time.Time) bool {
	data, err := os.ReadFile(filepath.Join(dir, LastRunFileName)) //nolint:gosec // a file of the pair's directory
	if err != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return err == nil && !t.Before(cutoff)
}

// removeAllExcept removes every entry of dir except keep.
func removeAllExcept(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() != keep {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// PruneStale removes the state of the pairs that have not run for
// Retention (Prune with Retention), and logs what it removed and any
// failure to log (nil discards). The worker calls it at start-up, then
// RunPruner daily.
func (s *Store) PruneStale(ctx context.Context, log *slog.Logger) {
	s.reporter(ctx, log)(s.Prune(ctx, Retention))
}

// RunPruner calls PruneStale every interval until ctx ends.
func (s *Store) RunPruner(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	s.pruneLoop(ctx, t.C, s.reporter(ctx, log))
}

// reporter returns the function that logs a pass of Prune.
func (s *Store) reporter(ctx context.Context, log *slog.Logger) func([]string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(removed []string, err error) {
		if len(removed) > 0 {
			log.Info("removed the state of two-way syncs that have not run for 180 days; their next run resyncs",
				"state_root", s.root, "pairs", len(removed))
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("could not remove the state of every two-way sync that has not run for 180 days; the next pass tries again",
				"state_root", s.root, "error", err.Error())
		}
	}
}

// pruneLoop prunes once per tick until ctx ends, and reports each pass.
func (s *Store) pruneLoop(ctx context.Context, ticks <-chan time.Time, report func(removed []string, err error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			report(s.Prune(ctx, Retention))
		}
	}
}
