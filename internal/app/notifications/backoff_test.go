package notifications

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second}, {1, 30 * time.Second}, {2, time.Minute}, {3, 2 * time.Minute}, {4, 4 * time.Minute},
		{5, 8 * time.Minute}, {6, 16 * time.Minute}, {7, 32 * time.Minute}, {8, time.Hour}, {9, time.Hour}, {1000, time.Hour},
	} {
		if got := retryDelay(tt.attempt); got != tt.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
	// The default 8 attempts span about an hour.
	var total time.Duration
	for a := 1; a < DefaultConfig().MaxAttempts; a++ {
		total += retryDelay(a)
	}
	if total < 50*time.Minute || total > 70*time.Minute {
		t.Errorf("the default retries span %v", total)
	}
}

func TestJitterBounds(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	draw := func() (uint64, bool) { return r.Uint64(), true }
	for _, d := range []time.Duration{BaseBackoff, time.Minute + time.Nanosecond, MaxBackoff, 3 * time.Nanosecond} {
		lo, hi := d, time.Duration(0)
		for range 10000 {
			j := jitter(d, draw)
			if j < d/2 || j > d {
				t.Fatalf("jitter(%v) = %v, outside [d/2, d]", d, j)
			}
			lo, hi = min(lo, j), max(hi, j)
		}
		if spread := hi - lo; spread < (d/2)*9/10 {
			t.Errorf("jitter(%v) spans only %v", d, spread)
		}
	}
	for _, v := range []uint64{0, 1, math.MaxUint64, uint64(BaseBackoff / 2)} {
		j := jitter(BaseBackoff, func() (uint64, bool) { return v, true })
		if j < BaseBackoff/2 || j > BaseBackoff {
			t.Errorf("jitter with draw %d = %v", v, j)
		}
	}
	if j := jitter(BaseBackoff, func() (uint64, bool) { return 0, false }); j != BaseBackoff {
		t.Errorf("jitter without randomness = %v, want the full delay", j)
	}
	// The extremes are reachable.
	if j := jitter(BaseBackoff, func() (uint64, bool) { return 0, true }); j != BaseBackoff/2 {
		t.Errorf("jitter(0) = %v", j)
	}
	if j := jitter(BaseBackoff, func() (uint64, bool) { return uint64(BaseBackoff - BaseBackoff/2), true }); j != BaseBackoff {
		t.Errorf("jitter(max) = %v", j)
	}
}
