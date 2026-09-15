package security

import (
	"sync"
	"time"
)

// RateLimiter is a simple per-key token bucket. Defaults match the TS impl:
// 60 tokens, refill 60/min (i.e. 1 token/sec amortized), enough for casual
// chat. The Check method consumes tokens and returns the time until the
// next allowed request — useful for surfacing back-off hints.
//
// Concurrency model: one mutex per limiter; per-key buckets behind a map.
// Sufficient for personal mode; team/enterprise can swap to a sharded
// implementation if profiling shows contention.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
}

type bucket struct {
	tokens  float64
	updated time.Time
}

// NewRateLimiter returns a limiter with the canonical defaults.
func NewRateLimiter() *RateLimiter {
	return NewRateLimiterWith(60, 1.0)
}

// NewRateLimiterWith customises capacity and per-second refill rate.
func NewRateLimiterWith(capacity, refillPerSec float64) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*bucket),
		capacity: capacity,
		refill:   refillPerSec,
	}
}

// Allow consumes one token for key. Returns true if the request is permitted
// and the wait duration until the next token if not.
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.capacity, updated: now}
		r.buckets[key] = b
	}
	elapsed := now.Sub(b.updated).Seconds()
	b.tokens = min(r.capacity, b.tokens+elapsed*r.refill)
	b.updated = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1-b.tokens)/r.refill*float64(time.Second)) + time.Millisecond
	return false, wait
}

// Reset clears the bucket for a key — used by tests / admin endpoints.
func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.buckets, key)
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
