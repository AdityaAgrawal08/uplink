package main

import (
	"sync"
	"time"
)

// RateLimiter implements a token-bucket rate limiter per key (burst-safe).
// Unlike the previous fixed window (which allowed 2x at the boundary),
// tokens refill continuously and bursts are capped at limit.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	limit    int
	windowSz time.Duration
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter creates a rate limiter with the given per-key limit
// per window, implemented as a token bucket.
func NewRateLimiter(limitPerSec int) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*bucket),
		limit:    limitPerSec,
		windowSz: time.Second,
	}
}

// Allow returns true if the key has not exceeded the rate limit.
// Keys are typically "sessionId:username" (chat), "chunk:..." (files),
// or "auth:sessionId" (password attempts).
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists {
		rl.buckets[key] = &bucket{
			tokens:   float64(rl.limit - 1),
			lastFill: now,
		}
		return true
	}
	// Refill proportional to elapsed time, capped at burst = limit.
	elapsed := now.Sub(b.lastFill).Seconds() / rl.windowSz.Seconds()
	b.tokens += elapsed * float64(rl.limit)
	if b.tokens > float64(rl.limit) {
		b.tokens = float64(rl.limit)
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Cleanup removes idle buckets. Called periodically.
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, b := range rl.buckets {
		if now.Sub(b.lastFill) > 5*rl.windowSz {
			delete(rl.buckets, k)
		}
	}
}
