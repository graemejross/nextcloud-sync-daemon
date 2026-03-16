// Package engine implements the unified event loop that coordinates sync triggers and execution.
package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

// Engine is the central coordinator. It owns the event channel, starts event sources,
// runs the main loop with cooldown enforcement, and handles graceful shutdown.
type Engine struct {
	executor daemon.SyncExecutor
	sources  []daemon.EventSource
	events   chan daemon.Event
	cooldown time.Duration
	logger   *slog.Logger
}

// New creates an Engine with the given executor, cooldown period, and event sources.
func New(executor daemon.SyncExecutor, cooldown time.Duration, logger *slog.Logger, sources ...daemon.EventSource) *Engine {
	return &Engine{
		executor: executor,
		sources:  sources,
		events:   make(chan daemon.Event, 1), // capacity 1 for coalescing
		cooldown: cooldown,
		logger:   logger,
	}
}

// Run starts all event sources and the main loop. Blocks until ctx is cancelled.
// On cancellation, waits for any in-flight sync to complete before returning.
func (e *Engine) Run(ctx context.Context) error {
	if len(e.sources) == 0 {
		return nil
	}

	// Start all event sources in goroutines
	var wg sync.WaitGroup
	for _, src := range e.sources {
		wg.Add(1)
		go func(s daemon.EventSource) {
			defer wg.Done()
			e.logger.Info("starting event source", "source", s.Name())
			if err := s.Start(ctx, e.events); err != nil {
				e.logger.Error("event source error", "source", s.Name(), "error", err)
			}
		}(src)
	}

	e.logger.Info("engine started",
		"sources", len(e.sources),
		"cooldown", e.cooldown,
	)

	// Main loop
	var lastSync time.Time
	var syncCount, failCount int64

	for {
		select {
		case <-ctx.Done():
			e.logger.Info("engine shutting down",
				"sync_count", syncCount,
				"fail_count", failCount,
			)
			// Wait for event sources to stop
			wg.Wait()
			return nil

		case event := <-e.events:
			// Enforce cooldown
			if elapsed := time.Since(lastSync); elapsed < e.cooldown && !lastSync.IsZero() {
				e.logger.Debug("event dropped (cooldown active)",
					"source", event.Source,
					"remaining", e.cooldown-elapsed,
				)
				continue
			}

			// Run sync
			e.logger.Info("sync triggered", "source", event.Source)
			result, err := e.executor.Run(ctx)

			if err != nil {
				failCount++
				e.logger.Error("sync execution error",
					"source", event.Source,
					"error", err,
					"fail_count", failCount,
				)
			} else if result.ExitCode != 0 {
				failCount++
				e.logger.Warn("sync completed with errors",
					"source", event.Source,
					"exit_code", result.ExitCode,
					"duration_ms", result.Duration.Milliseconds(),
					"fail_count", failCount,
				)
			} else {
				syncCount++
				e.logger.Info("sync complete",
					"source", event.Source,
					"duration_ms", result.Duration.Milliseconds(),
					"sync_count", syncCount,
				)
			}

			lastSync = time.Now()
		}
	}
}
