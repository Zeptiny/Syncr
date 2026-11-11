// Package datadir guards a worker's data directory: one worker process per
// directory (Lock), and a start without the leftovers of a crashed one
// (RemoveContents).
//
// Allowed dependencies: the standard library, internal/worker/filelock and
// internal/worker/proc.
package datadir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// LockFileName is the file in the data directory that a running worker
// holds locked.
const LockFileName = "worker.lock"

// ErrLocked reports that another worker process holds the data
// directory's lock.
var ErrLocked = errors.New("another worker is already running with this data directory")

// Lock takes the exclusive lock on <dataDir>/worker.lock, creating the
// directory (mode 0700) and the file if needed, without waiting. It fails
// with an error wrapping ErrLocked, which says what to do, when another
// process holds it. Two workers on one data directory would use the same
// scratch and cache directories and remove each other's job files. The
// worker holds the lock for its lifetime; call release when it stops. The operating system releases the lock if the process dies, so a
// crashed worker never leaves a stale lock.
func Lock(dataDir string) (release func() error, err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, LockFileName)
	release, ok, err := filelock.TryLock(path)
	if err != nil {
		return nil, fmt.Errorf("lock the data directory: %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w (%s, which holds %s): stop the other worker (its service, or the process started with \"run\"), "+
			"or give this one a data directory of its own (data_dir, SYNCR_DATA_DIR)", ErrLocked, dataDir, path)
	}
	return release, nil
}

// RemoveContents removes everything below each of dirs, leaving the
// directories themselves; a directory that does not exist is skipped. A
// worker calls it on its job scratch directories once it holds the data
// directory's Lock and before it accepts jobs, so the files of jobs a crash
// interrupted do not stay forever. Entries are removed with proc.RemoveAll,
// which waits briefly for Windows handles to close. It tries every entry
// and returns the failures joined; they are not fatal for a worker.
func RemoveContents(dirs ...string) error {
	var errs []error
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if err := proc.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
