package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"

	"golang.org/x/time/rate"
)

// Server is the main telnet server
type Server struct {
	config        *config.Config
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
	return &Server{
		config:  cfg,
		logger:  logger,
		metrics: m,
		rateLimiter: NewRateLimiter(
			cfg.RateLimit.Enabled,
			cfg.RateLimit.MaxAttempts,
			cfg.RateLimit.GetWindow(),
			cfg.RateLimit.GetBlockDuration(),
		),
		connTracker: NewConnectionTracker(
			cfg.Server.MaxConnections,
			cfg.Server.MaxPerIP,
		),
		acceptLimiter: rate.NewLimiter(rate.Limit(50), 20), // 50 accepts/sec, burst of 20
		bannerArt:     newBannerArt(cfg.Server.BannerDir),
		sessions:      make(map[net.Conn]*Session),
		done:          make(chan struct{}),
	}
}

// Start starts the server
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%d", s.config.Server.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = listener

	s.logger.Info().
		Str("event", "server_started").
		Int("port", s.config.Server.Port).
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

	// Create session
	sess := NewSession(conn, s.config, s.logger, s.bannerArt)
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
