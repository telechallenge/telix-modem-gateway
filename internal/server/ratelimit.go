package server

import (
	"sync"
	"time"
)

// RateLimiter implements per-IP rate limiting
type RateLimiter struct {
	mu sync.Mutex

	enabled       bool
	maxAttempts   int
	window        time.Duration
	blockDuration time.Duration

	attempts map[string][]time.Time
	blocked  map[string]time.Time
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(enabled bool, maxAttempts int, window, blockDuration time.Duration) *RateLimiter {
	rl := &RateLimiter{
		enabled:       enabled,
		maxAttempts:   maxAttempts,
		window:        window,
		blockDuration: blockDuration,
		attempts:      make(map[string][]time.Time),
		blocked:       make(map[string]time.Time),
	}

	// Start cleanup goroutine
	go rl.cleanup()

	return rl
}

// Allow checks if an IP is allowed to make a connection attempt
func (r *RateLimiter) Allow(ip string) bool {
	if !r.enabled {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Check if blocked
	if blockedUntil, ok := r.blocked[ip]; ok {
		if now.Before(blockedUntil) {
			return false
		}
		// Block expired
		delete(r.blocked, ip)
	}

	// Clean old attempts
	r.cleanAttemptsLocked(ip, now)

	// Check attempt count
	if len(r.attempts[ip]) >= r.maxAttempts {
		// Block the IP
		r.blocked[ip] = now.Add(r.blockDuration)
		delete(r.attempts, ip)
		return false
	}

	// Record this attempt atomically
	r.attempts[ip] = append(r.attempts[ip], now)
	return true
}

// RecordAttempt records a connection attempt from an IP
func (r *RateLimiter) RecordAttempt(ip string) {
	if !r.enabled {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.cleanAttemptsLocked(ip, now)
	r.attempts[ip] = append(r.attempts[ip], now)
}

// IsBlocked checks if an IP is currently blocked
func (r *RateLimiter) IsBlocked(ip string) bool {
	if !r.enabled {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if blockedUntil, ok := r.blocked[ip]; ok {
		if time.Now().Before(blockedUntil) {
			return true
		}
		delete(r.blocked, ip)
	}
	return false
}

func (r *RateLimiter) cleanAttemptsLocked(ip string, now time.Time) {
	attempts := r.attempts[ip]
	cutoff := now.Add(-r.window)

	// Find first valid attempt
	valid := 0
	for i, t := range attempts {
		if t.After(cutoff) {
			valid = i
			break
		}
		if i == len(attempts)-1 {
			valid = len(attempts)
		}
	}

	if valid > 0 {
		r.attempts[ip] = attempts[valid:]
	}
}

// cleanup periodically removes stale entries
func (r *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		r.mu.Lock()
		now := time.Now()

		// Clean expired blocks
		for ip, blockedUntil := range r.blocked {
			if now.After(blockedUntil) {
				delete(r.blocked, ip)
			}
		}

		// Clean old attempts
		cutoff := now.Add(-r.window)
		for ip, attempts := range r.attempts {
			valid := make([]time.Time, 0)
			for _, t := range attempts {
				if t.After(cutoff) {
					valid = append(valid, t)
				}
			}
			if len(valid) == 0 {
				delete(r.attempts, ip)
			} else {
				r.attempts[ip] = valid
			}
		}

		r.mu.Unlock()
	}
}

// ConnectionTracker tracks active connections per IP
type ConnectionTracker struct {
	mu sync.Mutex

	maxTotal int
	maxPerIP int
	conns    map[string]int
	total    int
}

// NewConnectionTracker creates a new connection tracker
func NewConnectionTracker(maxTotal, maxPerIP int) *ConnectionTracker {
	return &ConnectionTracker{
		maxTotal: maxTotal,
		maxPerIP: maxPerIP,
		conns:    make(map[string]int),
	}
}

// Add attempts to add a connection, returns false if limits exceeded
func (c *ConnectionTracker) Add(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.total >= c.maxTotal {
		return false
	}

	if c.conns[ip] >= c.maxPerIP {
		return false
	}

	c.conns[ip]++
	c.total++
	return true
}

// Remove removes a connection
func (c *ConnectionTracker) Remove(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conns[ip] > 0 {
		c.conns[ip]--
		c.total--
		if c.conns[ip] == 0 {
			delete(c.conns, ip)
		}
	}
}

// Count returns the total connection count
func (c *ConnectionTracker) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// CountForIP returns the connection count for an IP
func (c *ConnectionTracker) CountForIP(ip string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[ip]
}
