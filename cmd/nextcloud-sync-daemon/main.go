package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/engine"
	"github.com/graemejross/nextcloud-sync-daemon/internal/health"
	"github.com/graemejross/nextcloud-sync-daemon/internal/notifypush"
	"github.com/graemejross/nextcloud-sync-daemon/internal/peer"
	"github.com/graemejross/nextcloud-sync-daemon/internal/poller"
	"github.com/graemejross/nextcloud-sync-daemon/internal/sync"
	"github.com/graemejross/nextcloud-sync-daemon/internal/watcher"
	"github.com/graemejross/nextcloud-sync-daemon/internal/webhook"
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
		test        bool
	)

	flag.StringVar(&configPath, "config", "", "path to config file")
	flag.BoolVar(&once, "once", false, "run a single sync and exit")
	flag.BoolVar(&test, "test", false, "write a marker file, sync, report latency, clean up, and exit")
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

	// Security warnings
	if warn := cfg.CheckPasswordFilePermissions(); warn != "" {
		logger.Warn(warn)
	}
	if cfg.Health.Enabled && !strings.HasPrefix(cfg.Health.Listen, "127.0.0.1") && !strings.HasPrefix(cfg.Health.Listen, "localhost") {
		logger.Warn("health endpoint is accessible from the network — it exposes sync status information", "listen", cfg.Health.Listen)
	}

	// Check nextcloudcmd exists
	if err := sync.CheckNextcloudCmd(cfg.Sync.NextcloudCmd); err != nil {
		logger.Error("nextcloudcmd not available", "error", err)
		return 1
	}

	// Create executor
	executor := sync.NewExecutor(cfg, logger)
	ctx, stop := makeContext()
	defer stop()

	if test {
		return runSyncTest(cfg, executor, logger)
	}

	if once {
		// Run single sync and exit
		result, err := executor.Run(ctx)
		if err != nil {
			logger.Error("sync failed", "error", err)
			return 1
		}
		return result.ExitCode
	}

	// Daemon mode — require at least one event source
	if err := cfg.ValidateEventSources(); err != nil {
		logger.Error("config error", "error", err)
		return 1
	}

	// Always create health status — used by webhook and engine even if HTTP endpoint is disabled
	healthStatus := health.NewStatus()

	var sources []daemon.EventSource

	if cfg.Watch.Enabled {
		w, err := watcher.New(cfg.Sync.LocalDir, cfg.Watch.Debounce.Duration, cfg.Watch.Exclude, logger)
		if err != nil {
			logger.Error("failed to create watcher", "error", err)
			return 1
		}
		sources = append(sources, w)
	}

	if cfg.Webhook.Enabled {
		sources = append(sources, webhook.New(
			cfg.Webhook.Listen,
			cfg.Webhook.Secret,
			cfg.Webhook.PathFilter,
			logger,
			healthStatus,
		))
	}

	if cfg.Poll.Enabled {
		sources = append(sources, poller.New(cfg.Poll.Interval.Duration, logger))
	}

	if cfg.NotifyPush.Enabled {
		sources = append(sources, notifypush.New(cfg, logger, healthStatus))
	}

	// Health HTTP endpoint
	if cfg.Health.Enabled {
		healthSrv := &http.Server{
			Addr:    cfg.Health.Listen,
			Handler: healthStatus.Handler(),
		}
		go func() {
			logger.Info("health endpoint starting", "listen", cfg.Health.Listen)
			if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("health server error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = healthSrv.Shutdown(shutdownCtx)
		}()
	}

	// Peer notification
	var notifier daemon.PeerNotifier
	if len(cfg.Peers) > 0 {
		notifier = peer.New(cfg.Peers, logger, healthStatus)
		logger.Info("peer notification enabled", "peers", len(cfg.Peers))
	}

	eng := engine.New(executor, cfg.Watch.Cooldown.Duration, logger, healthStatus, notifier, sources...)

	// Systemd readiness notification — called after all sources start
	eng.OnReady = func() {
		_, _ = sdnotify.SdNotify(false, sdnotify.SdNotifyReady)
		logger.Info("daemon ready")
	}

	// Systemd watchdog — send heartbeats at half the watchdog interval
	if watchdogInterval, err := sdnotify.SdWatchdogEnabled(false); err == nil && watchdogInterval > 0 {
		go func() {
			ticker := time.NewTicker(watchdogInterval / 2)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = sdnotify.SdNotify(false, sdnotify.SdNotifyWatchdog)
				}
			}
		}()
	}

	logger.Info("starting daemon",
		"version", version,
		"poll", cfg.Poll.Enabled,
		"watch", cfg.Watch.Enabled,
		"webhook", cfg.Webhook.Enabled,
		"notify_push", cfg.NotifyPush.Enabled,
		"health", cfg.Health.Enabled,
	)

	if err := eng.Run(ctx); err != nil {
		logger.Error("engine error", "error", err)
		_, _ = sdnotify.SdNotify(false, sdnotify.SdNotifyStopping)
		return 1
	}

	_, _ = sdnotify.SdNotify(false, sdnotify.SdNotifyStopping)
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

// runSyncTest writes a marker file, syncs, reports latency, cleans up, and exits.
func runSyncTest(cfg *config.Config, executor daemon.SyncExecutor, logger *slog.Logger) int {
	ctx, stop := makeContext()
	defer stop()

	markerName := fmt.Sprintf(".nsd-sync-test-%d", time.Now().Unix())
	markerPath := filepath.Join(cfg.Sync.LocalDir, markerName)

	fmt.Printf("Sync test: writing marker file %s\n", markerName)
	if err := os.WriteFile(markerPath, []byte("nsd sync test\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing marker file: %v\n", err)
		return 1
	}

	start := time.Now()
	result, err := executor.Run(ctx)
	duration := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Sync failed: %v\n", err)
		_ = os.Remove(markerPath)
		return 1
	}

	fmt.Printf("Sync completed: exit code %d, duration %.1fs\n", result.ExitCode, duration.Seconds())

	if result.ExitCode != 0 {
		fmt.Fprintln(os.Stderr, "Sync test failed (non-zero exit code).")
		_ = os.Remove(markerPath)
		return 1
	}

	fmt.Println("Cleaning up marker file...")
	if err := os.Remove(markerPath); err != nil {
		logger.Warn("failed to remove marker file", "path", markerPath, "error", err)
	}

	// Second sync to propagate the deletion
	if _, err := executor.Run(ctx); err != nil {
		logger.Warn("cleanup sync failed", "error", err)
	}

	fmt.Println("Sync test passed.")
	return 0
}
