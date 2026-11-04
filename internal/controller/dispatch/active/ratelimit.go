package active

import (
	"sync"
	"time"
)

// RateLimit configures a RateLimiter: each key gets a token bucket of Burst
// tokens refilled at one token per Every.
type RateLimit struct {
	Every time.Duration
	Burst int
	// MaxKeys bounds the number of tracked keys. When it is reached, keys
	// whose bucket is full again are forgotten; if none is, new keys share
	// one overflow bucket, so a flood of distinct keys cannot grow memory
	// (it can only slow down the other new keys).
	MaxKeys int
}

// DefaultRateLimit allows a burst of 10 enrollments per client, then one
// every 6 seconds, for up to 10000 clients.
func DefaultRateLimit() RateLimit {
	return RateLimit{Every: 6 * time.Second, Burst: 10, MaxKeys: 10000}
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// RateLimiter is a Limiter with one token bucket per key. Create it with
// NewRateLimiter; it is safe for concurrent use.
type RateLimiter struct {
	cfg   RateLimit
	clock Clock

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// overflowKey is the shared bucket of new keys when MaxKeys is reached.
const overflowKey = "\x00overflow"

// NewRateLimiter returns a RateLimiter. Non-positive fields of cfg take the
// DefaultRateLimit values; a nil clock means the system clock.
func NewRateLimiter(cfg RateLimit, clock Clock) *RateLimiter {
	def := DefaultRateLimit()
	if cfg.Every <= 0 {
		cfg.Every = def.Every
	}
	if cfg.Burst <= 0 {
		cfg.Burst = def.Burst
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = def.MaxKeys
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &RateLimiter{cfg: cfg, clock: clock, buckets: make(map[string]*bucket)}
}

// Allow takes a token from key's bucket and reports whether there was one.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.cfg.MaxKeys {
			l.pruneLocked(now)
		}
		if len(l.buckets) >= l.cfg.MaxKeys {
			key = overflowKey
			b = l.buckets[key]
		}
		if b == nil {
			b = &bucket{tokens: float64(l.cfg.Burst), at: now}
			l.buckets[key] = b
		}
	}
	l.refill(b, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *RateLimiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(float64(l.cfg.Burst), b.tokens+float64(elapsed)/float64(l.cfg.Every))
		b.at = now
	}
}

// pruneLocked forgets keys whose bucket is full again.
func (l *RateLimiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		l.refill(b, now)
		if b.tokens >= float64(l.cfg.Burst) {
			delete(l.buckets, k)
		}
	}
}
