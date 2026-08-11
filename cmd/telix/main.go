package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/metrics"
	"telix/internal/probe"
	"telix/internal/server"
)

// publishPhonebook publishes the configured phone list as an inventory series,
// at startup and again after each config reload.
//
// It exists because the reachability gauges cannot stand in for it. Those only
// cover lines the prober dials, and busy lines are deliberately not dialled, so
// a phonebook of always-busy decoys reports fewer boards on the dashboard than
// the operator put in the file, with nothing to distinguish that from a probe
// that quietly stopped.
func publishPhonebook(cfg *config.Config, m *metrics.Metrics) {
	for _, entry := range cfg.Phonebook {
		// A busy line is not required to name a host, so it may have no
		// address at all. Empty is the honest label; JoinHostPort would render
		// that as ":0" and read like a real endpoint.
		address := ""
		if entry.Host != "" {
			address = net.JoinHostPort(entry.Host, strconv.Itoa(entry.Port))
		}
		m.PhonebookLine(entry.Name, address, entry.Busy)
	}
}

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

// startConfigWatcher wires the config file to the running gateway, so an edit
// takes effect without a restart and therefore without dropping the calls in
// progress. Returns nil when watching is disabled, which the caller may Stop
// unconditionally.
//
// The order below is the contract. The server is reloaded first so the next
// caller to dial gets the new phonebook at the earliest possible moment;
// metrics are republished from a clean slate so a deleted board stops reporting
// itself configured and reachable; the prober is rebuilt last because it is the
// only step that blocks, waiting out the probe cycle in flight.
func startConfigWatcher(
	path string,
	cfg *config.Config,
	srv *server.Server,
	logger *logging.Logger,
	m *metrics.Metrics,
	replaceProber func(*config.Config),
) *config.Watcher {
	if !cfg.Reload.Enabled {
		logger.Info().
			Str("event", "config_watch_disabled").
			Str("path", path).
			Msg("")
		return nil
	}

	onReload := func(next *config.Config) {
		ignored := srv.Reload(next)

		if m != nil {
			m.ResetPhonebookSeries()
			publishPhonebook(next, m)
		}
		replaceProber(next)

		event := logger.Info().
			Str("event", "config_reloaded").
			Str("path", path).
			Int("phonebook_entries", len(next.Phonebook))
		if len(ignored) > 0 {
			// Named explicitly, because a setting that changed in the file but
			// not in the process is the one outcome an operator would otherwise
			// never discover — they see "reloaded" and assume all of it landed.
			event.Str("needs_restart", strings.Join(ignored, ", "))
		}
		event.Msg("")
	}

	onError := func(err error) {
		// The gateway keeps running on the config it already has. A broken edit
		// costs the operator a log line, never a call.
		logger.Error().
			Str("event", "config_reload_failed").
			Str("path", path).
			Err(err).
			Msg("")
	}

	w := config.NewWatcher(path, cfg, onReload, onError)
	w.Start()

	logger.Info().
		Str("event", "config_watch_started").
		Str("path", path).
		Str("interval", cfg.Reload.GetInterval().String()).
		Msg("")

	return w
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
		publishPhonebook(cfg, m)
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

	// Reachability probing is started after the listener is up: the gateway's
	// job is answering calls, and telemetry must never delay that.
	prober := probe.New(cfg, logger, m)
	prober.Start()

	// The prober is replaced rather than mutated on reload — rebuilding it is
	// how the phonebook, the probe interval and the dialer's allowed_networks
	// all take effect at once, through the same constructor startup uses,
	// instead of through a second reload path that could drift from it.
	// Guarded by its own mutex because Stop blocks on the cycle in flight and
	// shutdown can arrive while a reload is doing exactly that.
	var proberMu sync.Mutex
	replaceProber := func(cfg *config.Config) {
		proberMu.Lock()
		defer proberMu.Unlock()
		prober.Stop()
		prober = probe.New(cfg, logger, m)
		prober.Start()
	}
	stopProber := func() {
		proberMu.Lock()
		defer proberMu.Unlock()
		prober.Stop()
	}

	watcher := startConfigWatcher(*configPath, cfg, srv, logger, m, replaceProber)

	fmt.Printf("Telix Modem Gateway v%s listening on telnet://0.0.0.0:%d\n", version, cfg.Server.Port)

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	logger.Info().
		Str("event", "shutdown_signal").
		Str("signal", sig.String()).
		Msg("")

	// Stop watching before tearing anything down, so a reload cannot land on
	// half-shut-down components — the watcher rebuilds the prober, and racing
	// that against shutdown would start one just as the last was stopped.
	if watcher != nil {
		watcher.Stop()
	}

	stopProber()

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
