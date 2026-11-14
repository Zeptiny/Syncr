// Package sourcestate keeps the worker's folders of backups of a remote
// (restic_backup with a remote source) below <data_dir>/sources (Root):
//
//	<root>/                     mode 0700
//	    .syncr-sources          MarkerName: tools that back up or copy a
//	                            folder holding <root> leave <root> out
//	    mirror/<source_key>/    one mirrored source, mode 0700
//	        lock                its lock: only the holder runs rclone or restic here
//	        last-run            when a run last used it, for retention
//	        data/               the copy of the remote's folder
//	    mount/<source_key>      the mount point of a mounted source
//	    mount/<source_key>.lock its lock
//
// A source key is 32 lower-case hex digits computed by the controller
// (link.ValidSourceKey). The store checks only its format, before it
// becomes a folder name.
//
// A mirrored run opens its mirror (Store.OpenMirror), which takes the lock
// without waiting and records the time in last-run; it closes it when it
// ends, which records the time again. Prune removes mirrors that have not
// been used for the retention the worker's configuration sets and whose
// lock is free; Open and Prune are serialized, so a mirror is never removed
// while a run takes it. A mounted run locks its mount point (Store.LockMount)
// for as long as it mounts there. The store never looks inside mount/
// beyond its own names: a stale FUSE mount blocks whoever touches it, and
// sweeping those is internal/worker/mount's work.
//
// Allowed dependencies: the standard library, internal/link (the source key
// format) and internal/worker/filelock.
package sourcestate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/filelock"
)

// Names inside the root and a mirror's folder.
const (
	// MarkerName is the file in the root that tells restic and rclone
	// (--exclude-if-present) to leave the root's contents out when they
	// back up or copy a folder that holds it.
	MarkerName      = ".syncr-sources"
	MirrorsDirName  = "mirror"
	MountsDirName   = "mount"
	LockFileName    = "lock"
	LastRunFileName = "last-run"
	DataDirName     = "data"
	// MountLockSuffix follows a source key in the name of a mount point's
	// lock file, next to the mount point.
	MountLockSuffix = ".lock"
)

// PruneInterval is how often a running worker prunes mirrors after its
// start-up pass (RunPruner).
const PruneInterval = 24 * time.Hour

// markerText is the content of the marker file, for whoever finds it.
const markerText = "This folder holds the copies and mount points of backups of remotes made by the Syncr worker.\n" +
	"Backups and copies of a folder that holds it leave it out. Do not change anything in it.\n"

// Sentinel errors, matched with errors.Is.
var (
	// ErrInvalidKey reports a source key that is not 32 lower-case hex
	// digits.
	ErrInvalidKey = errors.New("the source key must be 32 lower-case hex digits")
	// ErrBusy reports a mirror or mount point whose lock another run holds.
	ErrBusy = errors.New("another backup of this remote folder is running on this worker")
)

// Config configures a Store.
type Config struct {
	// Root is the absolute folder that holds the mirrors and mount points
	// (paths.SourcesDir). It is created (mode 0700) when first used.
	Root string
	// Now tells the time; nil means time.Now.
	Now func() time.Time
}

// Store is every mirror and mount point below one root. It is safe for
// concurrent use; one Store must serve every user of the root in the
// process (the runner and the pruner), because it serializes OpenMirror
// with Prune.
type Store struct {
	root string
	now  func() time.Time
	// mu is held while a mirror's lock is taken (OpenMirror) and while a
	// mirror is removed (Prune), never while a run uses its mirror.
	mu sync.Mutex
}

// New returns a Store for cfg. It touches nothing on disk.
func New(cfg Config) (*Store, error) {
	if !filepath.IsAbs(cfg.Root) {
		return nil, fmt.Errorf("sourcestate: root %q is not absolute", cfg.Root)
	}
	s := &Store{root: filepath.Clean(cfg.Root), now: cfg.Now}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Root returns the folder that holds the mirrors and mount points.
func (s *Store) Root() string { return s.root }

// MirrorsDir returns the folder that holds one folder per mirror.
func (s *Store) MirrorsDir() string { return filepath.Join(s.root, MirrorsDirName) }

// MountsDir returns the folder that holds the mount points.
func (s *Store) MountsDir() string { return filepath.Join(s.root, MountsDirName) }

// Prepare creates the root and dir below it (mode 0700) and writes the
// marker file into the root if it is missing.
func (s *Store) Prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	marker := filepath.Join(s.root, MarkerName)
	if _, err := os.Lstat(marker); err == nil {
		return nil
	}
	if err := os.WriteFile(marker, []byte(markerText), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", marker, err)
	}
	return nil
}

// Mirror is an open mirror: its lock is held until Close.
type Mirror struct {
	dir     string
	now     func() time.Time
	release func() error
	once    sync.Once
	err     error
}

// OpenMirror takes the lock of the mirror named key, without waiting,
// creating its folder (mode 0700) if needed, and records the time in
// last-run. It fails with ErrInvalidKey before key touches the file
// system, and with ErrBusy if another run holds the mirror. The caller must
// Close the mirror.
func (s *Store) OpenMirror(key string) (*Mirror, error) {
	if !link.ValidSourceKey(key) {
		return nil, ErrInvalidKey
	}
	dir := filepath.Join(s.MirrorsDir(), key)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Prepare(dir); err != nil {
		return nil, fmt.Errorf("create the mirror's folder: %w", err)
	}
	release, ok, err := filelock.TryLock(filepath.Join(dir, LockFileName))
	switch {
	case err != nil:
		return nil, fmt.Errorf("lock the mirror: %w", err)
	case !ok:
		return nil, ErrBusy
	}
	m := &Mirror{dir: dir, now: s.now, release: release}
	if err := m.stamp(); err != nil {
		_ = release()
		return nil, err
	}
	return m, nil
}

// Dir returns the mirror's folder.
func (m *Mirror) Dir() string { return m.dir }

// DataDir returns the folder that holds the copy of the remote's folder.
// It exists once rclone created it.
func (m *Mirror) DataDir() string { return filepath.Join(m.dir, DataDirName) }

// Close records the time in last-run and releases the mirror's lock. Later
// calls return the first result.
func (m *Mirror) Close() error {
	m.once.Do(func() { m.err = errors.Join(m.stamp(), m.release()) })
	return m.err
}

// stamp records the time in last-run.
func (m *Mirror) stamp() error {
	stamp := m.now().UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(filepath.Join(m.dir, LastRunFileName), []byte(stamp), 0o600); err != nil {
		return fmt.Errorf("record the run in the mirror's folder: %w", err)
	}
	return nil
}

// MountLock is a held mount point: its lock is held until Close.
type MountLock struct {
	point   string
	release func() error
	once    sync.Once
	err     error
}

// LockMount takes the lock of the mount point of key, without waiting,
// creating the mount points' folder (mode 0700) if needed, and returns the
// mount point (MountsDir/<key>), which it neither creates nor touches. It
// fails with ErrInvalidKey before key touches the file system, and with
// ErrBusy if another run holds it. The caller must Close the lock after it
// unmounted.
func (s *Store) LockMount(key string) (*MountLock, error) {
	if !link.ValidSourceKey(key) {
		return nil, ErrInvalidKey
	}
	if err := s.Prepare(s.MountsDir()); err != nil {
		return nil, fmt.Errorf("create the folder of mount points: %w", err)
	}
	release, ok, err := filelock.TryLock(filepath.Join(s.MountsDir(), key+MountLockSuffix))
	switch {
	case err != nil:
		return nil, fmt.Errorf("lock the mount point: %w", err)
	case !ok:
		return nil, ErrBusy
	}
	return &MountLock{point: filepath.Join(s.MountsDir(), key), release: release}, nil
}

// Point returns the mount point.
func (l *MountLock) Point() string { return l.point }

// Close releases the lock. Later calls return the first result.
func (l *MountLock) Close() error {
	l.once.Do(func() { l.err = l.release() })
	return l.err
}

// Prune removes the folder of every mirror whose last run is older than
// maxAge and whose lock is free, and returns the keys it removed. A mirror
// without a readable last-run counts from the modification time of that
// file, or of its folder. Entries that are not mirrors' folders are left
// alone, and nothing outside mirror/ is ever read. It stops early, with
// ctx's cause, when ctx ends.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) ([]string, error) {
	entries, err := os.ReadDir(s.MirrorsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the mirrors' folder: %w", err)
	}
	cutoff := s.now().Add(-maxAge)
	var removed []string
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, context.Cause(ctx))
			break
		}
		if !e.IsDir() || !link.ValidSourceKey(e.Name()) {
			continue
		}
		ok, err := s.pruneOne(e.Name(), cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("mirror %s: %w", e.Name(), err))
		}
		if ok {
			removed = append(removed, e.Name())
		}
	}
	return removed, errors.Join(errs...)
}

// pruneOne removes the mirror key if it last ran before cutoff and its
// lock is free, and reports whether it did.
func (s *Store) pruneOne(key string, cutoff time.Time) (bool, error) {
	dir := filepath.Join(s.MirrorsDir(), key)
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
	// A run may have opened and closed the mirror since the first look; it
	// recorded the time in last-run. (Taking the lock may have created the
	// lock file, so the folder's time no longer tells.)
	if ranSince(dir, cutoff) {
		return false, release()
	}
	// Everything but the lock file goes while the lock is held; Windows
	// does not delete a file that is open, so the lock file goes after the
	// release. OpenMirror waits on mu meanwhile.
	errContents := removeAllExcept(dir, LockFileName)
	if err := errors.Join(errContents, release()); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

// lastRun returns when the mirror in dir was last used: the time recorded
// in last-run, else that file's modification time, else the folder's.
func lastRun(dir string) (time.Time, error) {
	path := filepath.Join(dir, LastRunFileName)
	data, err := os.ReadFile(path) //nolint:gosec // a file of the mirror's folder
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
	data, err := os.ReadFile(filepath.Join(dir, LastRunFileName)) //nolint:gosec // a file of the mirror's folder
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

// PruneStale removes the mirrors that have not been used for maxAge (Prune)
// and logs what it removed and any failure (nil log discards). The worker
// calls it at start-up, then RunPruner daily.
func (s *Store) PruneStale(ctx context.Context, maxAge time.Duration, log *slog.Logger) {
	s.reporter(ctx, maxAge, log)(s.Prune(ctx, maxAge))
}

// RunPruner calls PruneStale every interval until ctx ends.
func (s *Store) RunPruner(ctx context.Context, every, maxAge time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	s.pruneLoop(ctx, t.C, maxAge, s.reporter(ctx, maxAge, log))
}

// reporter returns the function that logs a pass of Prune.
func (s *Store) reporter(ctx context.Context, maxAge time.Duration, log *slog.Logger) func([]string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(removed []string, err error) {
		if len(removed) > 0 {
			log.Info("deleted the mirrors of backups of remotes that have not run for remote_sources.mirror_retention; their next run downloads the folder again",
				"mirrors_dir", s.MirrorsDir(), "mirrors", len(removed), "retention", maxAge.String())
		}
		if err != nil && ctx.Err() == nil {
			log.Warn("could not delete every unused mirror of a backup of a remote; the next pass tries again",
				"mirrors_dir", s.MirrorsDir(), "error", err.Error())
		}
	}
}

// pruneLoop prunes once per tick until ctx ends, and reports each pass.
func (s *Store) pruneLoop(ctx context.Context, ticks <-chan time.Time, maxAge time.Duration, report func(removed []string, err error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			report(s.Prune(ctx, maxAge))
		}
	}
}
