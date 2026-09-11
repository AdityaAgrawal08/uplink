package main

import (
	"sync"
	"time"
)

// RateLimiter implements a sliding-window rate limiter per key.
type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string]*window
	limit    int
	windowSz time.Duration
}

type window struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter creates a rate limiter with the given per-key limit
// across a sliding window.
func NewRateLimiter(limitPerSec int) *RateLimiter {
	return &RateLimiter{
		windows:  make(map[string]*window),
		limit:    limitPerSec,
		windowSz: time.Second,
	}
}

// Allow returns true if the key has not exceeded the rate limit.
// Keys are typically "sessionId:username".
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	w, exists := rl.windows[key]
	if !exists || now.After(w.resetAt) {
		rl.windows[key] = &window{
			count:   1,
			resetAt: now.Add(rl.windowSz),
		}
		return true
	}
	if w.count >= rl.limit {
		return false
	}
	w.count++
	return true
}

// Cleanup removes expired windows. Called periodically.
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, w := range rl.windows {
		if now.After(w.resetAt) {
			delete(rl.windows, k)
		}
	}
}
