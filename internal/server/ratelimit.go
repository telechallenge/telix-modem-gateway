package server

import (
	"net"
	"sync"
	"time"
)

const maxTrackedIPs = 10000

// trustedNets are peer addresses that are not callers in their own right but
// reverse proxies standing in for many — the web terminal's telix-web
// container, in practice.
//
// Per-IP limiting is meaningless against such a peer and actively harmful: one
// address carries the whole browser population, so a per-IP ceiling becomes a
// site-wide one (max_per_ip: 5 capped the web terminal at five concurrent users
// in total) and a per-IP rate limit becomes a shared bucket that any six users
// in a minute can trip for everyone. Neither can ever identify an individual
// abuser, which is the only thing a per-IP limit is for.
//
// This does NOT make the exempted peer unlimited — the global ceilings still
// apply. The per-caller job on that path belongs to the layer that still knows
// who the caller is: MAX_WS_PER_IP in web/server.js, which reads the address
// nginx puts in X-Real-IP.
type trustedNets []*net.IPNet

// has reports whether ip is inside the trusted set. An unparseable address is
// never trusted, so a malformed peer fails closed into the normal limits.
func (t trustedNets) has(ip string) bool {
	if len(t) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range t {
		if n != nil && n.Contains(parsed) {
			return true
		}
	}
	return false
}

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
	trusted      trustedNets
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

// SetLimits updates the thresholds on a running limiter, so a config reload
// does not have to rebuild it and lose the in-flight attempt and block state
// with it — an operator raising a limit should not thereby forgive whoever is
// currently blocked.
func (r *RateLimiter) SetLimits(enabled bool, maxAttempts int, window, blockDuration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
	r.maxAttempts = maxAttempts
	r.window = window
	r.blockDuration = blockDuration
}

// SetTrustedProxies exempts these networks from per-IP rate limiting. Set at
// startup and on reload — it is deliberately not a constructor argument, so
// that adding it churned no existing call site.
func (r *RateLimiter) SetTrustedProxies(nets []*net.IPNet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trusted = nets
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

	// A trusted proxy skips the per-IP machinery below but is still counted
	// toward the global window above — it is exempt from being singled out as a
	// caller, not exempt from the ceiling that protects the gateway as a whole.
	// Deliberately placed after the global check for that reason.
	if r.trusted.has(ip) {
		r.globalCount++
		return true
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
	trusted  trustedNets
}

// NewConnectionTracker creates a new connection tracker
func NewConnectionTracker(maxTotal, maxPerIP int) *ConnectionTracker {
	return &ConnectionTracker{
		maxTotal: maxTotal,
		maxPerIP: maxPerIP,
		conns:    make(map[string]int),
	}
}

// SetLimits updates the ceilings on a running tracker. Connections already
// counted are left alone: lowering max_connections below the current count
// stops new callers rather than dropping the ones on the line, which is the
// only behaviour consistent with a reload that is not supposed to be felt.
func (c *ConnectionTracker) SetLimits(maxTotal, maxPerIP int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxTotal = maxTotal
	c.maxPerIP = maxPerIP
}

// SetTrustedProxies exempts these networks from the per-IP connection cap.
// Set at startup and on reload.
func (c *ConnectionTracker) SetTrustedProxies(nets []*net.IPNet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trusted = nets
}

// Add attempts to add a connection, returns false if limits exceeded
func (c *ConnectionTracker) Add(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.total >= c.maxTotal {
		return false
	}

	// maxTotal above still binds a trusted proxy; only the per-IP ceiling is
	// skipped, because that address is a whole population rather than a caller.
	if c.conns[ip] >= c.maxPerIP && !c.trusted.has(ip) {
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
