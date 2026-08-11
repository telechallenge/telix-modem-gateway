package probe

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"
)

// counter is a TCP listener that accepts and immediately hangs up, recording
// how many connections it took. The count is what proves a probe actually
// reached the wire — a reachable gauge alone cannot tell one dial from three.
type counter struct {
	ln net.Listener
	n  atomic.Int64
}

func listening(t *testing.T) *counter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c := &counter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.n.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

func (c *counter) host() string {
	h, _, _ := net.SplitHostPort(c.ln.Addr().String())
	return h
}

func (c *counter) port() int {
	_, p, _ := net.SplitHostPort(c.ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func (c *counter) address() string {
	return c.ln.Addr().String()
}

// closedPort returns an address nothing is listening on: a port is bound to
// learn a free number, then released.
func closedPort(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	n, _ := strconv.Atoi(p)
	return "127.0.0.1", n
}

func newProber(t *testing.T, cfg *config.Config, m *metrics.Metrics) *Prober {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	return New(cfg, logger, m)
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading scrape body: %v", err)
	}
	return string(body)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("scrape output missing %q\n--- got ---\n%s", w, body)
		}
	}
}

func TestProber_CycleRecordsReachabilityPerEntry(t *testing.T) {
	up := listening(t)
	downHost, downPort := closedPort(t)
	downAddr := net.JoinHostPort(downHost, strconv.Itoa(downPort))

	cfg := &config.Config{
		Probe: config.ProbeConfig{Enabled: true, Timeout: 2},
		Phonebook: []config.PhonebookEntry{
			{Number: "555-0001", Name: "Live BBS", Host: up.host(), Port: up.port()},
			{Number: "555-0002", Name: "Dead BBS", Host: downHost, Port: downPort},
		},
	}

	m := metrics.New()
	newProber(t, cfg, m).runOnce()

	mustContain(t, scrape(t, m),
		fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Live BBS"} 1`, up.address()),
		fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Dead BBS"} 0`, downAddr),
		fmt.Sprintf(`telix_bbs_probe_duration_seconds{address="%s",entry="Live BBS"}`, up.address()),
		fmt.Sprintf(`telix_bbs_last_probe_timestamp_seconds{address="%s",entry="Live BBS"}`, up.address()),
	)
}

// A busy entry never places an outbound call, so probing one measures a host
// no caller can reach through it — and config does not even require it to have
// a host and port.
func TestProber_SkipsBusyEntries(t *testing.T) {
	up := listening(t)

	cfg := &config.Config{
		Probe: config.ProbeConfig{Enabled: true, Timeout: 2},
		Phonebook: []config.PhonebookEntry{
			{Number: "555-0003", Name: "Engaged BBS", Host: up.host(), Port: up.port(), Busy: true},
			{Number: "555-0004", Name: "Hostless BBS", Busy: true},
		},
	}

	m := metrics.New()
	newProber(t, cfg, m).runOnce()

	body := scrape(t, m)
	if strings.Contains(body, "Engaged BBS") || strings.Contains(body, "Hostless BBS") {
		t.Errorf("a busy entry was probed\n--- got ---\n%s", body)
	}
	if n := up.n.Load(); n != 0 {
		t.Errorf("busy entry opened %d connections, want 0", n)
	}
}

// Half this phonebook points at one board under different numbers. Each entry
// still gets its own series — the dashboard names lines, not sockets — but the
// board must not be dialled once per name.
func TestProber_ProbesEachEndpointOncePerCycle(t *testing.T) {
	up := listening(t)

	cfg := &config.Config{
		Probe: config.ProbeConfig{Enabled: true, Timeout: 2},
		Phonebook: []config.PhonebookEntry{
			{Number: "555-0005", Name: "Board, front door", Host: up.host(), Port: up.port()},
			{Number: "555-0006", Name: "Board, back door", Host: up.host(), Port: up.port()},
		},
	}

	m := metrics.New()
	newProber(t, cfg, m).runOnce()

	if n := up.n.Load(); n != 1 {
		t.Errorf("endpoint took %d connections in one cycle, want 1", n)
	}
	mustContain(t, scrape(t, m),
		fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Board, front door"} 1`, up.address()),
		fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Board, back door"} 1`, up.address()),
	)
}

// The probe is an outbound connection like any other, so allowed_networks binds
// it. A blocked endpoint reads as unreachable rather than as an absent series —
// the operator wants to see that the number cannot be dialled.
func TestProber_HonoursAllowedNetworks(t *testing.T) {
	up := listening(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "telix.yaml")
	content := fmt.Sprintf(`
server:
  port: 2323
  max_connections: 10
  max_per_ip: 2
phonebook:
  - number: "555-0007"
    host: "%s"
    port: %d
    name: "Off-net BBS"
dialer:
  allowed_networks:
    - "10.0.0.0/8"
probe:
  enabled: true
  timeout: 2
`, up.host(), up.port())
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	m := metrics.New()
	newProber(t, cfg, m).runOnce()

	mustContain(t, scrape(t, m),
		fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Off-net BBS"} 0`, up.address()))
	if n := up.n.Load(); n != 0 {
		t.Errorf("a blocked endpoint took %d connections, want 0", n)
	}
}

// Start must probe straight away rather than waiting out the first interval, or
// the panel reads "No data" for a minute after every restart. Stop must return.
func TestProber_StartProbesImmediatelyAndStopReturns(t *testing.T) {
	up := listening(t)

	cfg := &config.Config{
		Probe:     config.ProbeConfig{Enabled: true, Timeout: 2},
		Phonebook: []config.PhonebookEntry{{Number: "555-0008", Name: "Live BBS", Host: up.host(), Port: up.port()}},
	}

	m := metrics.New()
	p := newProber(t, cfg, m)
	p.Start()
	defer p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	want := fmt.Sprintf(`telix_bbs_reachable{address="%s",entry="Live BBS"} 1`, up.address())
	for {
		if strings.Contains(scrape(t, m), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Start() did not probe within 5s\n--- got ---\n%s", scrape(t, m))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A disabled prober must be inert but still safe to Start and Stop, so main
// does not need to guard the calls.
func TestProber_DisabledDoesNothing(t *testing.T) {
	up := listening(t)

	cfg := &config.Config{
		Probe:     config.ProbeConfig{Enabled: false},
		Phonebook: []config.PhonebookEntry{{Number: "555-0009", Name: "Live BBS", Host: up.host(), Port: up.port()}},
	}

	m := metrics.New()
	p := newProber(t, cfg, m)
	p.Start()
	p.Stop()

	if n := up.n.Load(); n != 0 {
		t.Errorf("a disabled prober opened %d connections, want 0", n)
	}
	if strings.Contains(scrape(t, m), "telix_bbs_reachable") {
		t.Error("a disabled prober published reachability series")
	}
}
