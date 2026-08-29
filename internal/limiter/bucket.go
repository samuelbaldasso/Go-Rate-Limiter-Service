// Package limiter implements a per-key token bucket rate limiter using only
// the Go standard library.
package limiter

import (
	"context"
	"sync"
	"time"
)

// visitor holds the token bucket state for a single key (e.g. client IP).
type visitor struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// Limiter is a token-bucket rate limiter keyed by an arbitrary string (IP,
// API key, etc). Rate is expressed in tokens per second; Burst is the
// maximum number of tokens (and the number of tokens a new visitor starts
// with).
type Limiter struct {
	mu       sync.RWMutex
	visitors map[string]*visitor

	rate  float64
	burst float64
}

// New creates a Limiter that allows `rate` requests per second per key, with
// bursts of up to `burst` requests.
func New(rate, burst float64) *Limiter {
	return &Limiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		burst:    burst,
	}
}

// getVisitor returns the visitor for key, creating it if necessary.
func (l *Limiter) getVisitor(key string) *visitor {
	l.mu.RLock()
	v, ok := l.visitors[key]
	l.mu.RUnlock()
	if ok {
		return v
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-check: another goroutine may have created it while we waited for
	// the write lock.
	if v, ok = l.visitors[key]; ok {
		return v
	}
	v = &visitor{
		tokens:   l.burst,
		lastSeen: time.Now(),
	}
	l.visitors[key] = v
	return v
}

// Allow reports whether a request identified by key should be permitted,
// consuming one token if so.
func (l *Limiter) Allow(key string) bool {
	v := l.getVisitor(key)

	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(v.lastSeen).Seconds()
	v.lastSeen = now

	v.tokens += elapsed * l.rate
	if v.tokens > l.burst {
		v.tokens = l.burst
	}

	if v.tokens < 1 {
		return false
	}
	v.tokens--
	return true
}

// StartCleanup runs a background goroutine that periodically removes
// visitors that have been inactive for longer than ttl. It stops when ctx
// is canceled.
func (l *Limiter) StartCleanup(ctx context.Context, interval, ttl time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.cleanup(ttl)
			}
		}
	}()
}

func (l *Limiter) cleanup(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)

	l.mu.Lock()
	defer l.mu.Unlock()

	for key, v := range l.visitors {
		v.mu.Lock()
		lastSeen := v.lastSeen
		v.mu.Unlock()

		if lastSeen.Before(cutoff) {
			delete(l.visitors, key)
		}
	}
}
