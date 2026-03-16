package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/engine"
	"github.com/graemejross/nextcloud-sync-daemon/internal/poller"
	"github.com/graemejross/nextcloud-sync-daemon/internal/sync"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	var (
		configPath  string
		once        bool
		validate    bool
		showVersion bool
	)

	flag.StringVar(&configPath, "config", "", "path to config file")
	flag.BoolVar(&once, "once", false, "run a single sync and exit")
	flag.BoolVar(&validate, "validate", false, "validate config and exit")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("nextcloud-sync-daemon %s\n", version)
		return 0
	}

	// Find config file
	cfgPath, err := config.FindConfigPath(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Load and validate config
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if validate {
		fmt.Printf("config %s is valid\n", cfgPath)
		return 0
	}

	// Setup logging
	logger := setupLogging(cfg)
	logger.Info("loaded config", "path", cfgPath)

	// Check nextcloudcmd exists
	if err := sync.CheckNextcloudCmd(cfg.Sync.NextcloudCmd); err != nil {
		logger.Error("nextcloudcmd not available", "error", err)
		return 1
	}

	// Create executor
	executor := sync.NewExecutor(cfg, logger)
	ctx := makeContext()

	if once {
		// Run single sync and exit
		result, err := executor.Run(ctx)
		if err != nil {
			logger.Error("sync failed", "error", err)
			return 1
		}
		return result.ExitCode
	}

	// Daemon mode — build event sources and run engine
	var sources []daemon.EventSource

	if cfg.Poll.Enabled {
		sources = append(sources, poller.New(cfg.Poll.Interval.Duration, logger))
	}

	// Future: watcher and webhook sources will be added here in Phases 3 and 4

	eng := engine.New(executor, cfg.Watch.Cooldown.Duration, logger, sources...)

	logger.Info("starting daemon",
		"version", version,
		"poll", cfg.Poll.Enabled,
		"watch", cfg.Watch.Enabled,
		"webhook", cfg.Webhook.Enabled,
	)

	if err := eng.Run(ctx); err != nil {
		logger.Error("engine error", "error", err)
		return 1
	}

	logger.Info("daemon stopped")
	return 0
}

func setupLogging(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Logging.Format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	return slog.New(handler)
}
