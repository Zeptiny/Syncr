package tools

import (
	"context"
	"fmt"
	"time"
)

// lockPollInterval is how often a blocked acquirer retries.
const lockPollInterval = 50 * time.Millisecond

// tryLockFunc attempts to take an exclusive lock on the file at path without
// blocking. It returns ok=false if another holder has it. The release
// function must be called exactly once. The OS releases the lock if the
// process dies, so there are no stale locks.
type tryLockFunc func(path string) (release func() error, ok bool, err error)

// acquireLock takes an exclusive cross-process lock on the file at path,
// creating it if needed, and waits for other holders until ctx ends.
func acquireLock(ctx context.Context, path string) (release func() error, err error) {
	return acquireWith(ctx, path, tryLock)
}

func acquireWith(ctx context.Context, path string, try tryLockFunc) (func() error, error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		release, ok, err := try(path)
		if err != nil {
			return nil, fmt.Errorf("tools: lock %s: %w", path, err)
		}
		if ok {
			return release, nil
		}
		timer.Reset(lockPollInterval)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("tools: waiting for lock %s: %w", path, context.Cause(ctx))
		case <-timer.C:
		}
	}
}
