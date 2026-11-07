// Package ratelimit is a keyed token-bucket rate limiter with bounded
// memory: each key (a client IP, a username) gets a bucket of Burst tokens
// that refills at one token per Every.
//
// A bucket that has refilled completely is indistinguishable from a new
// one, so the limiter forgets such buckets when it needs room. If MaxKeys
// buckets are all still draining, new keys share one overflow bucket: a
// flood of distinct keys cannot grow memory, it can only slow down the other
// new keys.
//
// Allowed dependencies: the standard library only.
package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// Clock tells the time; clock.System and clock.Fake satisfy it.
type Clock interface {
	Now() time.Time
}

// Config configures a Limiter. Every field is required.
type Config struct {
	// Burst is the bucket size: how many calls a key may make at once.
	Burst int
	// Every is the refill interval: one token per Every.
	Every time.Duration
	// MaxKeys bounds the number of tracked keys (the overflow bucket comes
	// on top).
	MaxKeys int
}

// Validate reports a non-positive field.
func (c Config) Validate() error {
	if c.Burst < 1 || c.Every <= 0 || c.MaxKeys < 1 {
		return fmt.Errorf("ratelimit: invalid config: burst %d, every %v, max keys %d: all must be positive", c.Burst, c.Every, c.MaxKeys)
	}
	return nil
}

// Limiter is a keyed token-bucket rate limiter. Create it with New; it is
// safe for concurrent use.
type Limiter struct {
	cfg   Config
	clock Clock

	mu      sync.Mutex
	buckets map[string]*bucket
	// overflow is the bucket shared by new keys while the map is full.
	overflow *bucket
	// nextPrune is the earliest time a full map is scanned again for
	// refilled buckets. Buckets refill at one token per Every, so scanning
	// more often than that mostly finds nothing; without the throttle, every
	// call with a new key would scan the whole map.
	nextPrune time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New returns a Limiter. A nil clock means the system clock.
func New(cfg Config, clock Clock) (*Limiter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Limiter{cfg: cfg, clock: clock, buckets: make(map[string]*bucket)}, nil
}

// Allow takes a token from key's bucket and reports whether there was one.
// Keys are kept as given: callers bound their length.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.cfg.MaxKeys && !now.Before(l.nextPrune) {
			l.pruneLocked(now)
		}
		if len(l.buckets) < l.cfg.MaxKeys {
			b = l.fresh(now)
			l.buckets[key] = b
		} else {
			if l.overflow == nil {
				l.overflow = l.fresh(now)
			}
			b = l.overflow
		}
	}
	l.refill(b, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Len returns the number of tracked keys (for tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *Limiter) fresh(now time.Time) *bucket {
	return &bucket{tokens: float64(l.cfg.Burst), at: now}
}

func (l *Limiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(float64(l.cfg.Burst), b.tokens+float64(elapsed)/float64(l.cfg.Every))
		b.at = now
	}
}

// pruneLocked forgets the buckets that are full again. If none is, the map
// is not scanned again for one refill interval.
func (l *Limiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		l.refill(b, now)
		if b.tokens >= float64(l.cfg.Burst) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.cfg.MaxKeys {
		l.nextPrune = now.Add(l.cfg.Every)
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
