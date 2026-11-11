// Package filelock provides exclusive, cross-process locks on files, which
// the operating system releases when the holding process dies, so a lock is
// never stale.
//
// Allowed dependencies: the standard library.
package filelock

import (
	"context"
	"fmt"
	"time"
)

// pollInterval is how often a blocked Acquire retries.
const pollInterval = 50 * time.Millisecond

// tryFunc attempts to take an exclusive lock on the file at path without
// blocking (TryLock).
type tryFunc func(path string) (release func() error, ok bool, err error)

// TryLock takes an exclusive lock on the file at path, creating the file if
// needed, without waiting. It returns ok=false if another holder has the
// lock (another process, or another open of the file in this one). The
// release function must be called exactly once; the OS also releases the
// lock if the process dies.
func TryLock(path string) (release func() error, ok bool, err error) {
	return tryLock(path)
}

// Acquire takes an exclusive lock on the file at path, creating it if
// needed, and waits for other holders until ctx ends.
func Acquire(ctx context.Context, path string) (release func() error, err error) {
	return acquireWith(ctx, path, tryLock)
}

func acquireWith(ctx context.Context, path string, try tryFunc) (func() error, error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		release, ok, err := try(path)
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			return release, nil
		}
		timer.Reset(pollInterval)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for lock %s: %w", path, context.Cause(ctx))
		case <-timer.C:
		}
	}
}
