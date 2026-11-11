// Package datadir guards a worker's data directory: one worker process per
// directory (Lock), and a start without the leftovers of a crashed one
// (RemoveContents).
//
// Allowed dependencies: the standard library, internal/worker/filelock and
// internal/worker/proc.
package datadir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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
// directory (mode 0700) and the file if needed. Two workers on one data
// directory would use the same scratch and cache directories and remove
// each other's job files. The worker holds the lock for its lifetime; call
// release when it stops. The operating system releases the lock when the
// process dies, so a crashed worker never leaves a stale lock.
//
// When another process holds the lock, Lock calls waiting (if not nil)
// once, with the lock file's path, and keeps trying for up to wait. A
// service manager may start a worker while the process it replaces is
// still exiting: Task Scheduler's End and the Windows Service Control
// Manager report the old instance stopped before its process is gone, and
// that process still holds the lock for a moment. A second worker that
// really runs on the same directory still holds it after wait, and Lock
// then fails with an error wrapping ErrLocked that says what to do. With
// wait 0 it does not wait. If ctx ends first, Lock returns ctx's error
// (context.Cause), not ErrLocked.
func Lock(ctx context.Context, dataDir string, wait time.Duration, waiting func(lockFile string)) (release func() error, err error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, LockFileName)
	release, ok, err := filelock.TryLock(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("lock the data directory: %s: %w", path, err)
	case ok:
		return release, nil
	case wait <= 0:
		return nil, lockedError(dataDir, path, 0)
	}
	if waiting != nil {
		waiting(path)
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	release, err = filelock.Acquire(wctx, path)
	switch {
	case err == nil:
		return release, nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("waiting for the data directory lock %s: %w", path, context.Cause(ctx))
	case wctx.Err() != nil:
		return nil, lockedError(dataDir, path, wait)
	default:
		return nil, fmt.Errorf("lock the data directory: %w", err)
	}
}

// lockedError is ErrLocked with what to do; waited is how long Lock waited
// for the other process (0: not at all).
func lockedError(dataDir, path string, waited time.Duration) error {
	still := ""
	if waited > 0 {
		still = fmt.Sprintf(", and still did after %s", waited)
	}
	return fmt.Errorf("%w (%s; another process holds %s%s): stop the other worker (its service, or the process started with \"run\"), "+
		"or give this one a data directory of its own (data_dir, SYNCR_DATA_DIR)", ErrLocked, dataDir, path, still)
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
