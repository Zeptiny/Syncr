package agent

import (
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// throttle limits the Progress events of one job to one per interval
// without losing the newest sample:
//
//   - a sample arriving at least interval after the last published one is
//     published at once;
//   - otherwise it is kept (replacing an older kept sample) and published
//     when interval has passed since the last publication (trailing edge),
//     unless a newer sample replaces it first;
//   - close publishes a kept sample immediately, so the job's final sample is
//     delivered before its result.
//
// emit is called by the runner's goroutine; the trailing timer runs on its
// own goroutine, so a mutex guards the state. publish runs under that mutex,
// which keeps publications in order.
type throttle struct {
	interval time.Duration
	publish  func(runner.Progress)

	mu        sync.Mutex
	last      time.Time // when the last sample was published
	published bool      // a sample was published
	pending   *runner.Progress
	timer     *time.Timer
	timerGen  uint64          // identifies the current timer; stale callbacks do nothing
	latest    runner.Progress // newest sample seen
	seen      bool
	closed    bool
}

func newThrottle(interval time.Duration, publish func(runner.Progress)) *throttle {
	return &throttle{interval: interval, publish: publish}
}

// emit is the runner.EmitFunc of the job.
func (t *throttle) emit(p runner.Progress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return // the runner broke its contract and emitted after returning
	}
	t.latest, t.seen = p, true
	now := time.Now()
	if !t.published || now.Sub(t.last) >= t.interval {
		t.publishLocked(p, now)
		return
	}
	t.pending = &p
	if t.timer == nil {
		t.timerGen++
		gen := t.timerGen
		t.timer = time.AfterFunc(t.interval-now.Sub(t.last), func() { t.flush(gen) })
	}
}

// flush is the trailing-edge timer callback of timer generation gen. A
// callback that lost the race against Stop (its timer was replaced or
// stopped) does nothing.
func (t *throttle) flush(gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.timerGen || t.timer == nil {
		return
	}
	t.timer = nil
	if t.closed || t.pending == nil {
		return
	}
	t.publishLocked(*t.pending, time.Now())
}

func (t *throttle) publishLocked(p runner.Progress, now time.Time) {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.pending = nil
	t.last, t.published = now, true
	t.publish(p)
}

// close publishes a kept sample, stops the throttle and returns the newest
// sample seen, if any.
func (t *throttle) close() (runner.Progress, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && t.pending != nil {
		t.publishLocked(*t.pending, time.Now())
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.closed = true
	return t.latest, t.seen
}
