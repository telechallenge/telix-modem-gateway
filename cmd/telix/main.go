package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/server"
)

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

	// Create and start server
	srv := server.New(cfg, logger)
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

	srv.Stop()
}
