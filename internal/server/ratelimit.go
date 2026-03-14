package server

import (
	"sync"
	"time"
)

const maxTrackedIPs = 10000

// RateLimiter implements per-IP rate limiting
type RateLimiter struct {
	mu sync.Mutex

	enabled       bool
	maxAttempts   int
	window        time.Duration
	blockDuration time.Duration

	attempts     map[string][]time.Time
	blocked      map[string]time.Time
	globalCount  int       // total allows in the current window
	globalWindow time.Time // start of the current global window
	done         chan struct{}
}

// globalMaxRate is the maximum total allows per window across all IPs.
// If exceeded, all new connections are rejected regardless of per-IP limits.
const globalMaxRate = 1000

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(enabled bool, maxAttempts int, window, blockDuration time.Duration) *RateLimiter {
	rl := &RateLimiter{
		enabled:       enabled,
		maxAttempts:   maxAttempts,
		window:        window,
		blockDuration: blockDuration,
		attempts:      make(map[string][]time.Time),
		blocked:       make(map[string]time.Time),
		globalWindow:  time.Now(),
		done:          make(chan struct{}),
	}

	// Start cleanup goroutine
	go rl.cleanup()

	return rl
}

// Stop terminates the cleanup goroutine
func (r *RateLimiter) Stop() {
	close(r.done)
}

// Allow checks if an IP is allowed to make a connection attempt
func (r *RateLimiter) Allow(ip string) bool {
	if !r.enabled {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Check global rate — reject all if threshold exceeded
	if now.Sub(r.globalWindow) > r.window {
		r.globalCount = 0
		r.globalWindow = now
	}
	if r.globalCount >= globalMaxRate {
		return false
	}

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

	// Evict oldest entry if map is at capacity
	if len(r.attempts) >= maxTrackedIPs {
		if _, exists := r.attempts[ip]; !exists {
			r.evictOldestLocked()
		}
	}

	// Check attempt count
	if len(r.attempts[ip]) >= r.maxAttempts {
		// Block the IP
		r.blocked[ip] = now.Add(r.blockDuration)
		delete(r.attempts, ip)
		return false
	}

	// Record this attempt atomically
	r.attempts[ip] = append(r.attempts[ip], now)
	r.globalCount++
	return true
}

// evictOldestLocked removes the entry with the oldest last-attempt time.
// Must be called with r.mu held.
func (r *RateLimiter) evictOldestLocked() {
	var oldestIP string
	var oldestTime time.Time
	first := true
	for ip, attempts := range r.attempts {
		if len(attempts) == 0 {
			delete(r.attempts, ip)
			return
		}
		last := attempts[len(attempts)-1]
		if first || last.Before(oldestTime) {
			oldestIP = ip
			oldestTime = last
			first = false
		}
	}
	if oldestIP != "" {
		delete(r.attempts, oldestIP)
	}
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
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
		}

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
