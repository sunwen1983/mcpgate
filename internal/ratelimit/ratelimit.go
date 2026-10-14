// Package ratelimit implements per-key token-bucket rate limiting.
//
// A bucket holds up to Burst tokens and refills at Rate tokens per second.
// Each request consumes one token; a request with no token available is
// rejected. The clock is injectable so tests can control time.
package ratelimit

import (
	"sync"
	"time"
)

// Clock returns the current time. Tests replace it with a fake.
type Clock func() time.Time

// bucket is a single token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter holds one token bucket per key.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens refilled per second
	burst   int     // bucket capacity
	buckets map[string]*bucket
	now     Clock
}

// New builds a Limiter. rate must be > 0 and burst > 0.
func New(rate float64, burst int) *Limiter {
	return NewWithClock(rate, burst, time.Now)
}

// NewWithClock builds a Limiter using now as its clock.
func NewWithClock(rate float64, burst int, now Clock) *Limiter {
	return &Limiter{
		rate:    rate,
		burst:   burst,
		buckets: map[string]*bucket{},
		now:     now,
	}
}

// Allow reports whether key may proceed, consuming one token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}
	// Refill proportionally to elapsed time, capped at the burst size.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
