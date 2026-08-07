package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"
	"telix/internal/server"
)

// startMetricsServer brings up the Prometheus endpoint on its own listener,
// separate from the telnet port so a modem session can never reach it. Returns
// nil when metrics are disabled, in which case the gateway runs with a nil
// *metrics.Metrics and every recording call is a no-op.
//
// A failure to bind is logged rather than fatal: losing telemetry should not
// take a working BBS gateway offline.
func startMetricsServer(cfg *config.Config, logger *logging.Logger, m *metrics.Metrics) *http.Server {
	if !cfg.Metrics.Enabled {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	// Cheap liveness endpoint for compose healthchecks — the telnet port
	// speaks a protocol curl cannot, so this is the only HTTP-shaped probe.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              cfg.Metrics.Addr(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error().
				Str("event", "metrics_server_failed").
				Err(err).
				Msg("")
		}
	}()

	logger.Info().
		Str("event", "metrics_server_started").
		Str("addr", cfg.Metrics.Addr()).
		Msg("")

	return srv
}

var version = "1.0.0"

func main() {
	configPath := flag.String("config", "configs/telix.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Telix Modem Gateway v%s\n", version)
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfg.Version = version

	// Initialize logger
	logger, err := logging.New(cfg.Logging.Level, cfg.Logging.Format, cfg.Logging.File)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	logger.Info().
		Str("event", "startup").
		Str("version", version).
		Int("phonebook_entries", len(cfg.Phonebook)).
		Msg("")

	// Metrics are optional; a nil *Metrics makes every recording call a no-op.
	var m *metrics.Metrics
	if cfg.Metrics.Enabled {
		m = metrics.New()
		m.SetBuildInfo(version)
	}
	metricsSrv := startMetricsServer(cfg, logger, m)

	// Create and start server
	srv := server.NewWithMetrics(cfg, logger, m)
	if err := srv.Start(); err != nil {
		logger.Error().
			Str("event", "startup_failed").
			Err(err).
			Msg("")
		os.Exit(1)
	}

	fmt.Printf("Telix Modem Gateway v%s listening on telnet://0.0.0.0:%d\n", version, cfg.Server.Port)

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	logger.Info().
		Str("event", "shutdown_signal").
		Str("signal", sig.String()).
		Msg("")

	if metricsSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsSrv.Shutdown(ctx); err != nil {
			logger.Warn().
				Str("event", "metrics_shutdown_error").
				Err(err).
				Msg("")
		}
	}

	srv.Stop()
}
