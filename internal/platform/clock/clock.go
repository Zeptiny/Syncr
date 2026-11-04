// Package clock provides time sources: System for production and Fake for
// deterministic tests. Consumers declare their own one-method port
// (interface{ Now() time.Time }); both types satisfy it.
//
// Only "now" is abstracted. Code that waits (long polls, loops) uses the time
// package directly and is tested with testing/synctest, whose bubbles fake
// timers too.
//
// Allowed dependencies: the standard library.
package clock

import (
	"sync"
	"time"
)

// Precision is the resolution of the times both clocks return: microseconds,
// the resolution of a Postgres timestamptz, so a time survives a round trip
// through any store unchanged and compares equal afterwards.
const Precision = time.Microsecond

// System reads the system clock. The zero value is ready to use.
type System struct{}

// Now returns the current time in UTC, truncated to Precision and without a
// monotonic clock reading.
func (System) Now() time.Time { return time.Now().UTC().Truncate(Precision) }

// Fake is a manually advanced clock for tests. It is safe for concurrent
// use; create it with NewFake.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake clock reading start (normalized like System).
func NewFake(start time.Time) *Fake {
	return &Fake{now: start.UTC().Truncate(Precision)}
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the clock to t (which may be in the past).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC().Truncate(Precision)
}

// Advance moves the clock forward by d and returns the new time.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d).Truncate(Precision)
	return f.now
}
