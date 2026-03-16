// Package poller implements a timer-based EventSource that triggers syncs at a fixed interval.
package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

// Poller sends sync events at a fixed interval. The simplest EventSource.
type Poller struct {
	interval time.Duration
	logger   *slog.Logger
}

// New creates a Poller with the given interval.
func New(interval time.Duration, logger *slog.Logger) *Poller {
	return &Poller{interval: interval, logger: logger}
}

// Start begins sending events at the configured interval. Blocks until ctx is cancelled.
func (p *Poller) Start(ctx context.Context, trigger chan<- daemon.Event) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.logger.Info("poller started", "interval", p.interval)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("poller stopped")
			return nil
		case t := <-ticker.C:
			event := daemon.Event{
				Source: "poller",
				Time:   t,
			}
			select {
			case trigger <- event:
				p.logger.Debug("poll event sent")
			default:
				p.logger.Debug("poll event dropped (sync already pending)")
			}
		}
	}
}

// Name returns the source name for logging.
func (p *Poller) Name() string {
	return "poller"
}
