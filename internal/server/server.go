package server

import (
	"fmt"
	"net"
	"sync"

	"telix/internal/config"
	"telix/internal/logging"
)

// Server is the main telnet server
type Server struct {
	config      *config.Config
	logger      *logging.Logger
	listener    net.Listener
	rateLimiter *RateLimiter
	connTracker *ConnectionTracker

	mu       sync.Mutex
	sessions map[net.Conn]*Session
	done     chan struct{}
}

// New creates a new server
func New(cfg *config.Config, logger *logging.Logger) *Server {
	return &Server{
		config: cfg,
		logger: logger,
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
		sessions: make(map[net.Conn]*Session),
		done:     make(chan struct{}),
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

	s.logger.Info().
		Str("event", "server_stopped").
		Msg("")
}

func (s *Server) acceptLoop() {
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
		conn.Close()
		return
	}

	s.logger.NewConnection(host)

	// Create session
	sess := NewSession(conn, s.config, s.logger)

	s.mu.Lock()
	s.sessions[conn] = sess
	s.mu.Unlock()

	// Run session
	sess.Run()

	// Cleanup
	s.mu.Lock()
	delete(s.sessions, conn)
	s.mu.Unlock()

	s.connTracker.Remove(host)
	s.logger.Disconnected(host)
}

// ConnectionCount returns the current connection count
func (s *Server) ConnectionCount() int {
	return s.connTracker.Count()
}
