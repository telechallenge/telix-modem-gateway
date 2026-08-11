package server

import (
	"net"
	"testing"

	"telix/internal/config"
	"telix/internal/logging"
)

func reloadTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	s := New(cfg, logger)
	t.Cleanup(s.rateLimiter.Stop)
	return s
}

func phonebookConfig(entries ...config.PhonebookEntry) *config.Config {
	return &config.Config{
		Server:    config.ServerConfig{Port: 2323, MaxConnections: 100, MaxPerIP: 5},
		Phonebook: entries,
	}
}

// The behaviour the whole feature rests on: an edit reaches the next caller,
// and cannot disturb the one already on the line.
func TestServerReload_NewCallersSeeTheEditAndLiveOnesDoNot(t *testing.T) {
	first := config.PhonebookEntry{Number: "5551234", Host: "first.example.com", Port: 23, Name: "First"}
	second := config.PhonebookEntry{Number: "5555678", Host: "second.example.com", Port: 23, Name: "Second"}

	t.Run("a number added by the edit becomes dialable", func(t *testing.T) {
		s := reloadTestServer(t, phonebookConfig(first))

		if s.Config().LookupNumber("5555678") != nil {
			t.Fatal("second number should not resolve before the reload")
		}
		s.Reload(phonebookConfig(first, second))

		entry := s.Config().LookupNumber("5555678")
		if entry == nil {
			t.Fatal("second number should resolve after the reload")
		}
		if entry.Host != "second.example.com" {
			t.Errorf("host = %q, want second.example.com", entry.Host)
		}
	})

	t.Run("a session started before the reload keeps its own phonebook", func(t *testing.T) {
		s := reloadTestServer(t, phonebookConfig(first))

		// Capture the config the way handleConnection does, then edit the
		// number out from under the running session.
		client, srvConn := net.Pipe()
		t.Cleanup(func() { client.Close(); srvConn.Close() })
		sess := NewSession(srvConn, s.Config(), s.logger, s.bannerArt)

		s.Reload(phonebookConfig(second))

		if sess.config.LookupNumber("5551234") == nil {
			t.Error("a live session must keep the phonebook it connected under")
		}
		if s.Config().LookupNumber("5551234") != nil {
			t.Error("the deleted number must be gone for new callers")
		}
	})
}

func TestServerReload_ReportsWhatItCouldNotApply(t *testing.T) {
	base := func() *config.Config {
		c := phonebookConfig()
		c.Logging = config.LoggingConfig{Level: "info", Format: "json", File: "/var/log/telix.log"}
		c.Metrics = config.MetricsConfig{Enabled: true, Port: 9101}
		return c
	}

	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"port change needs a restart", func(c *config.Config) { c.Server.Port = 2424 }, "server.port"},
		{"log file change needs a restart", func(c *config.Config) { c.Logging.File = "/tmp/other.log" }, "logging.file"},
		{"log format change needs a restart", func(c *config.Config) { c.Logging.Format = "text" }, "logging.format"},
		{"metrics port change needs a restart", func(c *config.Config) { c.Metrics.Port = 9999 }, "metrics"},
		{"metrics disable needs a restart", func(c *config.Config) { c.Metrics.Enabled = false }, "metrics"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := reloadTestServer(t, base())
			next := base()
			tt.mutate(next)

			ignored := s.Reload(next)

			if len(ignored) != 1 || ignored[0] != tt.want {
				t.Errorf("ignored = %v, want exactly [%s]", ignored, tt.want)
			}
		})
	}

	t.Run("an edit that only touches reloadable settings reports nothing", func(t *testing.T) {
		s := reloadTestServer(t, base())
		next := base()
		next.Phonebook = []config.PhonebookEntry{{Number: "5551234", Host: "h", Port: 23}}
		next.Server.MaxPerIP = 50

		if ignored := s.Reload(next); len(ignored) != 0 {
			t.Errorf("expected nothing to need a restart, got %v", ignored)
		}
	})
}

// Version is set from the build stamp at runtime and never appears in YAML, so
// a reload that took the parsed config at face value would blank it — and the
// banner, ATI and the Grafana Version panel with it.
func TestServerReload_CarriesTheBuildVersion(t *testing.T) {
	cfg := phonebookConfig()
	cfg.Version = "1.4.2"
	s := reloadTestServer(t, cfg)

	s.Reload(phonebookConfig())

	if got := s.Config().Version; got != "1.4.2" {
		t.Errorf("Version = %q after reload, want 1.4.2", got)
	}
}

// The ceilings live in the limiter and tracker, which copied them at
// construction — so a reload that only swapped the config pointer would leave
// them on the old values with nothing to say so.
func TestServerReload_AppliesLimitsToTheRunningLimiters(t *testing.T) {
	cfg := phonebookConfig()
	cfg.Server.MaxPerIP = 1
	cfg.RateLimit = config.RateLimitConfig{Enabled: false}
	s := reloadTestServer(t, cfg)

	if !s.connTracker.Add("10.0.0.1") {
		t.Fatal("first connection should be allowed")
	}
	if s.connTracker.Add("10.0.0.1") {
		t.Fatal("second connection should hit max_per_ip of 1")
	}

	raised := phonebookConfig()
	raised.Server.MaxPerIP = 5
	raised.RateLimit = config.RateLimitConfig{Enabled: false}
	s.Reload(raised)

	if !s.connTracker.Add("10.0.0.1") {
		t.Error("raising max_per_ip should admit the next connection without a restart")
	}
}

func TestServerReload_MovesTheBannerDirectory(t *testing.T) {
	cfg := phonebookConfig()
	cfg.Server.BannerDir = "/etc/telix/banners"
	s := reloadTestServer(t, cfg)

	next := phonebookConfig()
	next.Server.BannerDir = "/etc/telix/other"
	s.Reload(next)

	if got := s.bannerArt.dir(); got != "/etc/telix/other" {
		t.Errorf("banner dir = %q, want /etc/telix/other", got)
	}
}
