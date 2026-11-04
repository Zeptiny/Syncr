package tools

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockExcludesAndReleases(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".lock")

	release, err := acquireLock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}

	// While held, a second acquirer blocks until its context ends.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := acquireLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire while held: err = %v, want deadline exceeded", err)
	}

	// Once released, it succeeds promptly.
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release2, err := acquireLock(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := release2(); err != nil {
		t.Fatal(err)
	}
}

func TestLockSerializesCriticalSections(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".lock")
	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
		wg      sync.WaitGroup
	)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireLock(t.Context(), path)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			maxSeen = max(maxSeen, inside)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			if err := release(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Errorf("%d holders inside the critical section at once", maxSeen)
	}
}

func TestAcquireWithReportsErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	_, err := acquireWith(t.Context(), "x", func(string) (func() error, bool, error) { return nil, false, boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}
