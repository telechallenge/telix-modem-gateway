package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"

	"golang.org/x/time/rate"
)

// Server is the main telnet server
type Server struct {
	// config is swapped wholesale by Reload rather than mutated, which is what
	// keeps the session hot path lock-free and free of torn reads: a session
	// captures the pointer once at NewSession and keeps dialling the phonebook
	// it connected under, while the next caller gets the edited one. A call in
	// progress is never re-pointed underneath itself.
	config        atomic.Pointer[config.Config]
	logger        *logging.Logger
	metrics       *metrics.Metrics // nil when metrics are disabled; all calls are no-ops
	listener      net.Listener
	rateLimiter   *RateLimiter
	connTracker   *ConnectionTracker
	acceptLimiter *rate.Limiter // throttles TCP accept rate to prevent flood attacks
	bannerArt     *bannerArt    // ANSI art greeting; falls back to the built-in banner

	mu       sync.Mutex
	sessions map[net.Conn]*Session
	done     chan struct{}
}

// New creates a new server
func New(cfg *config.Config, logger *logging.Logger) *Server {
	return NewWithMetrics(cfg, logger, nil)
}

// NewWithMetrics creates a server that reports to m. A nil m disables metrics.
func NewWithMetrics(cfg *config.Config, logger *logging.Logger, m *metrics.Metrics) *Server {
	rateLimiter := NewRateLimiter(
		cfg.RateLimit.Enabled,
		cfg.RateLimit.MaxAttempts,
		cfg.RateLimit.GetWindow(),
		cfg.RateLimit.GetBlockDuration(),
	)
	connTracker := NewConnectionTracker(
		cfg.Server.MaxConnections,
		cfg.Server.MaxPerIP,
	)

	// Both per-IP limits share one exemption list, because they share the
	// defect it corrects: a reverse proxy is a single peer standing in for many
	// callers, so anything keyed on its address is a shared bucket. Applied
	// here rather than at each check in handleConnection so a later edit cannot
	// wire up one and miss the other.
	trusted := cfg.Server.ParsedTrustedProxies()
	rateLimiter.SetTrustedProxies(trusted)
	connTracker.SetTrustedProxies(trusted)

	s := &Server{
		logger:        logger,
		metrics:       m,
		rateLimiter:   rateLimiter,
		connTracker:   connTracker,
		acceptLimiter: rate.NewLimiter(rate.Limit(50), 20), // 50 accepts/sec, burst of 20
		bannerArt:     newBannerArt(cfg.Server.BannerDir),
		sessions:      make(map[net.Conn]*Session),
		done:          make(chan struct{}),
	}
	s.config.Store(cfg)
	return s
}

// Config returns the configuration new callers will be served. Existing
// sessions hold their own pointer and are unaffected by a later reload.
func (s *Server) Config() *config.Config {
	return s.config.Load()
}

// Reload swaps in an edited configuration and returns the names of any settings
// that changed but could not be applied to the running process.
//
// Returning them rather than logging here is deliberate: the caller already
// owns the reload log line, and a setting that was silently ignored is the
// worst outcome of a hot reload — the operator edits a value, sees a success
// message, and never learns the gateway is still running the old one.
func (s *Server) Reload(cfg *config.Config) []string {
	old := s.config.Load()

	// The listener is already bound, so a port change cannot take effect
	// without dropping every caller — which is exactly what a hot reload
	// exists to avoid. Report it and keep serving the port we are on.
	var ignored []string
	if old.Server.Port != cfg.Server.Port {
		ignored = append(ignored, "server.port")
	}
	if old.Logging.File != cfg.Logging.File {
		ignored = append(ignored, "logging.file")
	}
	if old.Logging.Format != cfg.Logging.Format {
		// Switching mid-stream would leave one log file holding two formats,
		// which breaks any parser pointed at it.
		ignored = append(ignored, "logging.format")
	}
	if old.Metrics.Enabled != cfg.Metrics.Enabled || old.Metrics.GetPort() != cfg.Metrics.GetPort() || old.Metrics.Bind != cfg.Metrics.Bind {
		ignored = append(ignored, "metrics")
	}

	// Version is set at runtime from the build stamp, never from YAML, so it
	// has to be carried across or ATI and the banner would report "" after the
	// first reload.
	cfg.Version = old.Version

	// Push the values the limiters copied at construction. They are set before
	// the pointer swap so a caller arriving mid-reload meets the new ceilings
	// with the new phonebook, never a mix of new phonebook and old ceilings.
	trusted := cfg.Server.ParsedTrustedProxies()
	s.rateLimiter.SetLimits(
		cfg.RateLimit.Enabled,
		cfg.RateLimit.MaxAttempts,
		cfg.RateLimit.GetWindow(),
		cfg.RateLimit.GetBlockDuration(),
	)
	s.rateLimiter.SetTrustedProxies(trusted)
	s.connTracker.SetLimits(cfg.Server.MaxConnections, cfg.Server.MaxPerIP)
	s.connTracker.SetTrustedProxies(trusted)
	s.bannerArt.SetDir(cfg.Server.BannerDir)

	s.logger.SetLevel(cfg.Logging.Level)

	s.config.Store(cfg)
	return ignored
}

// Start starts the server
func (s *Server) Start() error {
	port := s.Config().Server.Port
	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = listener

	s.logger.Info().
		Str("event", "server_started").
		Int("port", port).
		Msg("")

	go s.acceptLoop()
	return nil
}

// Stop stops the server
func (s *Server) Stop() {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}

	s.mu.Lock()
	for conn, sess := range s.sessions {
		sess.Close()
		delete(s.sessions, conn)
	}
	s.mu.Unlock()

	s.rateLimiter.Stop()

	s.logger.Info().
		Str("event", "server_stopped").
		Msg("")
}

func (s *Server) acceptLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-s.done
		cancel()
	}()

	for {
		select {
		case <-s.done:
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				s.logger.Error().
					Str("event", "accept_error").
					Err(err).
					Msg("")
				continue
			}
		}

		// Throttle accept rate to prevent TCP flood attacks from
		// overwhelming the server with goroutine creation.
		if err := s.acceptLimiter.Wait(ctx); err != nil {
			conn.Close()
			return
		}

		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	remoteAddr := conn.RemoteAddr().String()
	host, _, _ := net.SplitHostPort(remoteAddr)
	if host == "" {
		host = remoteAddr
	}

	// Check rate limit atomically (checks block, records attempt, blocks if exceeded)
	if !s.rateLimiter.Allow(host) {
		s.logger.RateLimited(host, "blocked")
		s.metrics.ConnectionRejected(metrics.ReasonRateLimited)
		conn.Close()
		return
	}

	// Check connection limits
	if !s.connTracker.Add(host) {
		s.logger.Warn().
			Str("event", "connection_rejected").
			Str("source_ip", host).
			Str("reason", "limit_exceeded").
			Msg("")
		s.metrics.ConnectionRejected(metrics.ReasonLimitExceeded)
		conn.Close()
		return
	}

	// Create session. The config pointer is read once here and held for the
	// call's lifetime, so an edit landing mid-call cannot change the phonebook
	// under a caller who is already connected to a board.
	sess := NewSession(conn, s.Config(), s.logger, s.bannerArt)
	sess.metrics = s.metrics
	sess.logger.NewConnection()

	s.mu.Lock()
	s.sessions[conn] = sess
	s.mu.Unlock()

	// Run session
	started := time.Now()
	s.metrics.SessionStarted()
	sess.Run()
	s.metrics.SessionEnded(time.Since(started))

	// Cleanup
	s.mu.Lock()
	delete(s.sessions, conn)
	s.mu.Unlock()

	s.connTracker.Remove(host)
	sess.logger.Disconnected()
}
