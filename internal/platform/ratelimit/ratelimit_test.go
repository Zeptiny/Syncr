package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

func start() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }

func newLimiter(t *testing.T, cfg Config) (*Limiter, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(start())
	l, err := New(cfg, clk)
	if err != nil {
		t.Fatal(err)
	}
	return l, clk
}

// allowN calls Allow n times and returns how many were allowed.
func allowN(l *Limiter, key string, n int) int {
	ok := 0
	for range n {
		if l.Allow(key) {
			ok++
		}
	}
	return ok
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	valid := Config{Burst: 1, Every: time.Second, MaxKeys: 1}
	if _, err := New(valid, nil); err != nil {
		t.Errorf("New(%+v): %v", valid, err)
	}
	for _, cfg := range []Config{
		{},
		{Burst: 0, Every: time.Second, MaxKeys: 1},
		{Burst: 1, Every: 0, MaxKeys: 1},
		{Burst: 1, Every: -time.Second, MaxKeys: 1},
		{Burst: 1, Every: time.Second, MaxKeys: 0},
	} {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestBurstAndRefill(t *testing.T) {
	t.Parallel()
	l, clk := newLimiter(t, Config{Burst: 5, Every: time.Minute, MaxKeys: 10})
	if got := allowN(l, "a", 8); got != 5 {
		t.Fatalf("burst: %d allowed, want 5", got)
	}
	clk.Advance(59 * time.Second)
	if l.Allow("a") {
		t.Fatal("allowed before a token refilled")
	}
	clk.Advance(time.Second)
	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("want exactly one token after one interval")
	}
	// Refill is capped at the burst.
	clk.Advance(time.Hour)
	if got := allowN(l, "a", 10); got != 5 {
		t.Errorf("after a long pause: %d allowed, want 5", got)
	}
	// Keys are independent.
	if got := allowN(l, "b", 10); got != 5 {
		t.Errorf("other key: %d allowed, want 5", got)
	}
}

func TestClockGoingBackwards(t *testing.T) {
	t.Parallel()
	l, clk := newLimiter(t, Config{Burst: 1, Every: time.Minute, MaxKeys: 10})
	if !l.Allow("a") {
		t.Fatal("first call denied")
	}
	clk.Set(start().Add(-time.Hour))
	if l.Allow("a") {
		t.Error("a clock step back refilled the bucket")
	}
}

func TestMaxKeysForgetsRefilledBuckets(t *testing.T) {
	t.Parallel()
	l, clk := newLimiter(t, Config{Burst: 2, Every: time.Minute, MaxKeys: 3})
	for i := range 3 {
		l.Allow(fmt.Sprint("k", i))
	}
	if l.Len() != 3 {
		t.Fatalf("Len = %d, want 3", l.Len())
	}
	// The map is full and every bucket is draining: new keys share the
	// overflow bucket (burst 2), whatever their name.
	if !l.Allow("new1") || !l.Allow("new2") {
		t.Fatal("overflow bucket starts full")
	}
	if l.Allow("new3") {
		t.Error("the overflow bucket is shared by new keys")
	}
	if l.Len() != 3 {
		t.Errorf("Len = %d, want 3 (overflow keys are not tracked)", l.Len())
	}
	// Tracked keys keep their own buckets meanwhile.
	if !l.Allow("k0") {
		t.Error("a tracked key lost its bucket")
	}

	// Once buckets refill they are forgotten, making room for new keys.
	clk.Advance(2 * time.Minute)
	if !l.Allow("fresh") {
		t.Fatal("new key denied after buckets refilled")
	}
	if got := allowN(l, "fresh", 5); got != 1 {
		t.Errorf("fresh key has its own bucket: %d more allowed, want 1", got)
	}
}

func TestPruneIsThrottled(t *testing.T) {
	t.Parallel()
	l, clk := newLimiter(t, Config{Burst: 1, Every: time.Minute, MaxKeys: 2})
	l.Allow("a")
	l.Allow("b")
	// Full map, nothing refilled: the scan finds nothing and is not
	// repeated within the interval, so "c" and "d" share the overflow
	// bucket.
	if !l.Allow("c") {
		t.Fatal("overflow bucket starts full")
	}
	clk.Advance(30 * time.Second)
	if l.Allow("d") {
		t.Error("the overflow bucket was not shared")
	}
	clk.Advance(30 * time.Second)
	if !l.Allow("e") {
		t.Error("after one interval the refilled buckets are forgotten")
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	l, _ := newLimiter(t, Config{Burst: 100, Every: time.Hour, MaxKeys: 50})
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			n := allowN(l, "shared", 10)
			allowN(l, fmt.Sprint("own", i), 3)
			mu.Lock()
			allowed += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("%d calls allowed on a key of burst 100", allowed)
	}
}

func TestSystemClockDefault(t *testing.T) {
	t.Parallel()
	l, err := New(Config{Burst: 1, Every: time.Hour, MaxKeys: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allow("a") || l.Allow("a") {
		t.Error("system clock limiter does not limit")
	}
}
