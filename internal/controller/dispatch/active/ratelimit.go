package active

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
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

// RateLimiter is a Limiter with one token bucket per key: the keyed token
// bucket of internal/platform/ratelimit. Create it with NewRateLimiter; it
// is safe for concurrent use.
type RateLimiter struct {
	limiter *ratelimit.Limiter
}

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
	var c ratelimit.Clock
	if clock != nil {
		c = clock
	}
	l, err := ratelimit.New(ratelimit.Config{Burst: cfg.Burst, Every: cfg.Every, MaxKeys: cfg.MaxKeys}, c)
	if err != nil {
		// Unreachable: every field is positive after the defaults above.
		panic(err)
	}
	return &RateLimiter{limiter: l}
}

// Allow takes a token from key's bucket and reports whether there was one.
func (l *RateLimiter) Allow(key string) bool { return l.limiter.Allow(key) }
