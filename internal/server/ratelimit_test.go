package server

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// mustCIDR is a test helper: the trusted-proxy set is []*net.IPNet everywhere,
// and spelling out the error handling at every call site buries the intent.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("bad test CIDR %q: %v", s, err)
	}
	return n
}

// A reverse proxy is one TCP peer standing in for a whole population of
// callers, so per-IP limits applied to it are really shared buckets: they
// throttle every browser user together and can never identify one. The web
// terminal reaches the gateway as the telix-web container address, which is
// what made max_per_ip a site-wide cap on concurrent users. Direct callers must
// keep both limits — port 2323 faces the internet and takes real scanners.
func TestConnectionTracker_TrustedProxyExemptFromPerIPCapButNotTotal(t *testing.T) {
	proxy := mustCIDR(t, "172.18.0.0/16")

	ct := NewConnectionTracker(10, 2)
	ct.SetTrustedProxies([]*net.IPNet{proxy})

	for i := 0; i < 10; i++ {
		if !ct.Add("172.18.0.6") {
			t.Fatalf("trusted proxy rejected at connection %d: max_per_ip must not apply to it", i+1)
		}
	}
	if ct.Add("172.18.0.6") {
		t.Fatal("trusted proxy allowed past max_connections: the global ceiling must still hold")
	}

	direct := NewConnectionTracker(10, 2)
	direct.SetTrustedProxies([]*net.IPNet{proxy})
	if !direct.Add("203.0.113.9") || !direct.Add("203.0.113.9") {
		t.Fatal("direct caller rejected below max_per_ip")
	}
	if direct.Add("203.0.113.9") {
		t.Fatal("direct caller allowed past max_per_ip: exemption must not leak to untrusted peers")
	}
}

func TestRateLimiter_TrustedProxyNotRateLimitedButDirectCallerIs(t *testing.T) {
	proxy := mustCIDR(t, "172.18.0.0/16")

	rl := NewRateLimiter(true, 2, time.Minute, time.Minute)
	rl.SetTrustedProxies([]*net.IPNet{proxy})
	defer rl.Stop()

	for i := 0; i < 20; i++ {
		if !rl.Allow("172.18.0.6") {
			t.Fatalf("trusted proxy rate-limited at attempt %d", i+1)
		}
	}

	if !rl.Allow("203.0.113.9") || !rl.Allow("203.0.113.9") {
		t.Fatal("direct caller blocked below maxAttempts")
	}
	if rl.Allow("203.0.113.9") {
		t.Fatal("direct caller not blocked past maxAttempts: exemption must not leak to untrusted peers")
	}
}

// ---------------------------------------------------------------------------
// RateLimiter tests
// ---------------------------------------------------------------------------

func TestRateLimiter_AllowWhenDisabled(t *testing.T) {
	rl := NewRateLimiter(false, 1, time.Second, time.Second)
	defer rl.Stop()

	// Should always return true regardless of how many calls are made.
	for i := 0; i < 100; i++ {
		if !rl.Allow("192.168.1.1") {
			t.Fatalf("Allow() returned false on call %d with disabled limiter", i+1)
		}
	}
}

func TestRateLimiter_AllowWithinLimits(t *testing.T) {
	maxAttempts := 5
	rl := NewRateLimiter(true, maxAttempts, time.Minute, time.Minute)
	defer rl.Stop()

	for i := 0; i < maxAttempts; i++ {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("Allow() returned false on attempt %d, expected true (limit %d)", i+1, maxAttempts)
		}
	}
}

func TestRateLimiter_BlockAfterExceedingLimits(t *testing.T) {
	maxAttempts := 3
	rl := NewRateLimiter(true, maxAttempts, time.Minute, time.Minute)
	defer rl.Stop()

	ip := "10.0.0.2"

	// Use up all allowed attempts.
	for i := 0; i < maxAttempts; i++ {
		rl.Allow(ip)
	}

	// Next attempt should be blocked.
	if rl.Allow(ip) {
		t.Fatal("Allow() returned true after exceeding maxAttempts, expected false")
	}

	// Subsequent attempts should still be blocked (IP is now in blocked map).
	if rl.Allow(ip) {
		t.Fatal("Allow() returned true on second call after block, expected false")
	}
}

func TestRateLimiter_BlockExpiry(t *testing.T) {
	maxAttempts := 2
	blockDuration := 80 * time.Millisecond
	rl := NewRateLimiter(true, maxAttempts, time.Minute, blockDuration)
	defer rl.Stop()

	ip := "10.0.0.3"

	// Exhaust attempts and trigger block.
	for i := 0; i < maxAttempts; i++ {
		rl.Allow(ip)
	}
	if rl.Allow(ip) {
		t.Fatal("expected block after exhausting attempts")
	}

	// Wait for block to expire.
	time.Sleep(blockDuration + 20*time.Millisecond)

	if !rl.Allow(ip) {
		t.Fatal("Allow() returned false after block expiry, expected true")
	}
}

func TestRateLimiter_WindowExpiry(t *testing.T) {
	maxAttempts := 2
	window := 80 * time.Millisecond
	// Use a long block so we know window expiry is the mechanism, not block expiry.
	rl := NewRateLimiter(true, maxAttempts, window, 10*time.Minute)
	defer rl.Stop()

	ip := "10.0.0.4"

	// Use one attempt (below the limit so we don't trigger a block).
	if !rl.Allow(ip) {
		t.Fatal("first Allow() should succeed")
	}

	// Wait for the window to pass so the first attempt ages out.
	time.Sleep(window + 20*time.Millisecond)

	// Now we should have a clean slate; all maxAttempts should be available.
	for i := 0; i < maxAttempts; i++ {
		if !rl.Allow(ip) {
			t.Fatalf("Allow() returned false on attempt %d after window expiry", i+1)
		}
	}
}

func TestRateLimiter_GlobalRateLimit(t *testing.T) {
	// Create a limiter with a generous per-IP limit so per-IP limits don't kick in.
	rl := NewRateLimiter(true, globalMaxRate+100, time.Minute, time.Minute)
	defer rl.Stop()

	// Exhaust the global rate limit using distinct IPs (1 attempt each).
	for i := 0; i < globalMaxRate; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xFF, (i>>8)&0xFF, i&0xFF)
		if !rl.Allow(ip) {
			t.Fatalf("Allow() returned false at global count %d, expected true", i)
		}
	}

	// The next attempt from any IP should be rejected.
	if rl.Allow("192.168.99.99") {
		t.Fatal("Allow() returned true after global rate exceeded, expected false")
	}
}

func TestRateLimiter_DifferentIPsAreIndependent(t *testing.T) {
	maxAttempts := 2
	rl := NewRateLimiter(true, maxAttempts, time.Minute, time.Minute)
	defer rl.Stop()

	ipA := "10.0.0.10"
	ipB := "10.0.0.11"

	// Block IP A.
	for i := 0; i < maxAttempts; i++ {
		rl.Allow(ipA)
	}
	if rl.Allow(ipA) {
		t.Fatal("ipA should be blocked")
	}

	// IP B should be unaffected.
	for i := 0; i < maxAttempts; i++ {
		if !rl.Allow(ipB) {
			t.Fatalf("Allow() for ipB returned false on attempt %d; ipA block should not affect ipB", i+1)
		}
	}
}

func TestRateLimiter_StopDoesNotPanic(t *testing.T) {
	rl := NewRateLimiter(true, 5, time.Second, time.Second)

	// Stop should exit the cleanup goroutine without panicking.
	rl.Stop()

	// Give cleanup goroutine time to exit.
	time.Sleep(20 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// ConnectionTracker tests
// ---------------------------------------------------------------------------

func TestConnectionTracker_AddWithinLimits(t *testing.T) {
	ct := NewConnectionTracker(10, 5)

	if !ct.Add("10.0.0.1") {
		t.Fatal("Add() returned false within limits")
	}
	if ct.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", ct.Count())
	}
}

func TestConnectionTracker_AddExceedsTotalLimit(t *testing.T) {
	maxTotal := 3
	ct := NewConnectionTracker(maxTotal, 10)

	for i := 0; i < maxTotal; i++ {
		ip := fmt.Sprintf("10.0.0.%d", i)
		if !ct.Add(ip) {
			t.Fatalf("Add() returned false on connection %d, expected true", i+1)
		}
	}

	// One more should fail.
	if ct.Add("10.0.0.99") {
		t.Fatal("Add() returned true after exceeding maxTotal")
	}
}

func TestConnectionTracker_AddExceedsPerIPLimit(t *testing.T) {
	maxPerIP := 2
	ct := NewConnectionTracker(100, maxPerIP)

	ip := "10.0.0.1"
	for i := 0; i < maxPerIP; i++ {
		if !ct.Add(ip) {
			t.Fatalf("Add() returned false on per-IP connection %d", i+1)
		}
	}

	// Next add for same IP should fail.
	if ct.Add(ip) {
		t.Fatal("Add() returned true after exceeding maxPerIP")
	}

	// A different IP should still succeed.
	if !ct.Add("10.0.0.2") {
		t.Fatal("Add() for a different IP should succeed when only one IP is at its per-IP limit")
	}
}

func TestConnectionTracker_RemoveFreesSlots(t *testing.T) {
	maxTotal := 2
	ct := NewConnectionTracker(maxTotal, 10)

	ip := "10.0.0.1"
	ct.Add(ip)
	ct.Add("10.0.0.2")

	// At capacity — should fail.
	if ct.Add("10.0.0.3") {
		t.Fatal("expected Add() to fail at capacity")
	}

	// Remove one connection.
	ct.Remove(ip)

	// Now there's room.
	if !ct.Add("10.0.0.3") {
		t.Fatal("Add() should succeed after Remove freed a slot")
	}
}

func TestConnectionTracker_RemoveUnknownIP(t *testing.T) {
	ct := NewConnectionTracker(10, 5)

	// Remove on an IP that was never added should not panic or cause negative counts.
	ct.Remove("10.0.0.99")

	if ct.Count() != 0 {
		t.Fatalf("Count() = %d after removing unknown IP, want 0", ct.Count())
	}

	// Should still function correctly after the no-op remove.
	if !ct.Add("10.0.0.1") {
		t.Fatal("Add() should succeed after no-op Remove")
	}
	if ct.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", ct.Count())
	}
}

func TestConnectionTracker_CountAccuracy(t *testing.T) {
	ct := NewConnectionTracker(100, 50)

	tests := []struct {
		action string
		ip     string
		want   int
	}{
		{"add", "10.0.0.1", 1},
		{"add", "10.0.0.1", 2},
		{"add", "10.0.0.2", 3},
		{"remove", "10.0.0.1", 2},
		{"add", "10.0.0.3", 3},
		{"remove", "10.0.0.2", 2},
		{"remove", "10.0.0.1", 1},
		{"remove", "10.0.0.3", 0},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("step_%d_%s_%s", i, tc.action, tc.ip), func(t *testing.T) {
			switch tc.action {
			case "add":
				ct.Add(tc.ip)
			case "remove":
				ct.Remove(tc.ip)
			}
			if got := ct.Count(); got != tc.want {
				t.Fatalf("after %s(%s): Count() = %d, want %d", tc.action, tc.ip, got, tc.want)
			}
		})
	}
}
