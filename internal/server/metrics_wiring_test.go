package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"
	"telix/internal/modem"
)

// scrape renders m the way Prometheus would, so these tests assert on the text
// an operator's dashboard actually queries rather than on collector internals.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading scrape: %v", err)
	}
	return string(body)
}

func assertScrapeHas(t *testing.T, body string, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("scrape missing %q\n--- got ---\n%s", want, body)
	}
}

// metricsSession builds a session wired to a fresh registry, with the dial-tone
// wait zeroed so the S6 sleep does not make these tests take seconds each.
func metricsSession(t *testing.T, cfg *config.Config) (*Session, *metrics.Metrics) {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	s := &Session{
		modem:   modem.New("test"),
		config:  cfg,
		logger:  logger.WithSession("test-session", "127.0.0.1"),
		metrics: m,
	}
	s.modem.Registers().Set(6, 0) // no dial-tone wait
	return s, m
}

// The dial outcome labels are set by hand at a dozen exit points in dial.go, so
// the mapping from "what happened" to "what the dashboard shows" is exactly the
// thing that rots when someone adds a branch. Assert the mapping, not the
// plumbing — metrics_test.go already covers the counters themselves.
func TestHandleDial_RecordsOutcomeLabels(t *testing.T) {
	tests := []struct {
		name string
		dial func(s *Session)
		want string
	}{
		{
			name: "a number that is not in the phonebook",
			dial: func(s *Session) { s.handleDial("555-0000") },
			want: `telix_dials_total{entry="unknown",outcome="unknown_number"} 1`,
		},
		{
			name: "a dial inside the cooldown is refused without dialling",
			dial: func(s *Session) {
				// Arm the cooldown directly rather than by placing a real dial
				// first: sendIntercept sleeps a random 2000-3500ms and the
				// cooldown is 3000ms, so a preceding dial races the very
				// boundary under test and fails about a third of the time.
				s.lastDial = time.Now()
				s.handleDial("555-0000")
			},
			want: `telix_dials_total{entry="unknown",outcome="blocked"} 1`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Version: "test"}
			s, m := metricsSession(t, cfg)

			// handleDial writes result codes to the client; a session built
			// without a conn would panic, so give it a discard pipe.
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			s.conn = server
			go io.Copy(io.Discard, client)

			tt.dial(s)
			assertScrapeHas(t, scrape(t, m), tt.want)
		})
	}
}

// Every dial must leave the in-flight gauge where it found it. A path that
// increments without a matching decrement makes the gauge climb forever, which
// reads on the dashboard as a permanently wedged gateway.
func TestHandleDial_InFlightGaugeReturnsToZero(t *testing.T) {
	cfg := &config.Config{Version: "test"}
	s, m := metricsSession(t, cfg)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	s.conn = server
	go io.Copy(io.Discard, client)

	s.handleDial("555-0000")

	assertScrapeHas(t, scrape(t, m), "telix_dials_in_flight 0")
}

// The entry label must come from the phonebook name, never the dialled digits:
// a caller sweeping numbers would otherwise mint one time series per guess and
// eventually take Prometheus down. This is the cardinality guard.
func TestHandleDial_UnknownNumbersShareOneSeries(t *testing.T) {
	cfg := &config.Config{Version: "test"}
	s, m := metricsSession(t, cfg)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	s.conn = server
	go io.Copy(io.Discard, client)

	// Distinct numbers, all absent from the phonebook. The cooldown turns the
	// later ones into "blocked", so assert on what must NOT appear rather than
	// on a single count: no series may carry a dialled number.
	for _, n := range []string{"555-0001", "555-0002", "555-0003"} {
		s.handleDial(n)
	}

	body := scrape(t, m)
	for _, n := range []string{"555-0001", "555-0002", "555-0003"} {
		if strings.Contains(body, n) {
			t.Errorf("dialled number %q leaked into a metric label:\n%s", n, body)
		}
	}
}

// Rejections happen in Server.handleConnection, before a Session exists, so
// they are wired separately from everything above.
//
// These drive handleConnection itself rather than calling the recorder — an
// assertion on a hand-made ConnectionRejected call would still pass with the
// instrumentation deleted from handleConnection, which is the only thing worth
// testing here.
func TestServer_RecordsConnectionRejections(t *testing.T) {
	newServer := func(t *testing.T, rateLimit config.RateLimitConfig) (*Server, *metrics.Metrics) {
		t.Helper()
		logger, err := logging.New("error", "text", "")
		if err != nil {
			t.Fatal(err)
		}
		m := metrics.New()
		cfg := &config.Config{
			Server:    config.ServerConfig{Port: 0, MaxConnections: 1, MaxPerIP: 1},
			RateLimit: rateLimit,
		}
		srv := NewWithMetrics(cfg, logger, m)
		t.Cleanup(srv.rateLimiter.Stop)
		return srv, m
	}

	// net.Pipe's RemoteAddr has no host:port form, so handleConnection falls
	// back to using the whole address as the per-IP key. Pre-filling the
	// tracker under that same key is what makes the next connection exceed the
	// limit.
	pipeKey := func() string {
		c, other := net.Pipe()
		defer c.Close()
		defer other.Close()
		return c.RemoteAddr().String()
	}()

	t.Run("over the per-IP connection limit", func(t *testing.T) {
		srv, m := newServer(t, config.RateLimitConfig{Enabled: false})

		if !srv.connTracker.Add(pipeKey) {
			t.Fatal("priming Add was refused; test setup is wrong")
		}

		client, server := net.Pipe()
		defer client.Close()
		srv.handleConnection(server) // must return immediately, having rejected

		assertScrapeHas(t, scrape(t, m), `telix_connections_rejected_total{reason="limit_exceeded"} 1`)
	})

	t.Run("blocked by the rate limiter", func(t *testing.T) {
		// One attempt allowed per window, so the second connection is blocked.
		srv, m := newServer(t, config.RateLimitConfig{
			Enabled: true, MaxAttempts: 1, WindowSeconds: 60, BlockDuration: 60,
		})

		if !srv.rateLimiter.Allow(pipeKey) {
			t.Fatal("first Allow was refused; test setup is wrong")
		}

		client, server := net.Pipe()
		defer client.Close()
		srv.handleConnection(server)

		assertScrapeHas(t, scrape(t, m), `telix_connections_rejected_total{reason="rate_limited"} 1`)
	})
}
