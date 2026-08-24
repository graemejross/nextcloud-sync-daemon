// Package daemon defines shared types and interfaces for the sync daemon.
// All internal packages import daemon; daemon imports nothing from internal.
package daemon

import (
	"context"
	"time"
)

// Event represents a sync trigger from any source.
type Event struct {
	Source string    // "watcher", "webhook", "poller", "notify_push"
	Time   time.Time // when the event was generated
	Path   string    // file path that changed (empty for poll events)
}

// SyncResult captures the outcome of a single sync run.
type SyncResult struct {
	StartTime time.Time
	Duration  time.Duration
	ExitCode  int
	Trigger   string // what caused this sync
	Error     error

	// Failure describes the item that failed the sync, recovered from
	// nextcloudcmd's stderr (Refs #44). All three are empty when the sync
	// succeeded or when stderr carried nothing recognisable.
	FailReason string // SyncFileItem::Status token, e.g. "BlacklistedError"
	FailPath   string // path of the failing item, control characters escaped
	FailDetail string // the client's error string for that item
}

// EventSource produces sync trigger events. Each trigger layer implements this.
type EventSource interface {
	// Start begins producing events. Blocks until ctx is cancelled.
	Start(ctx context.Context, trigger chan<- Event) error
	// Name returns a human-readable name for logging.
	Name() string
}

// SyncExecutor runs nextcloudcmd and reports the result.
type SyncExecutor interface {
	// Run executes a sync. Returns when the sync completes.
	Run(ctx context.Context) (*SyncResult, error)
}

// PeerNotifier sends sync notifications to peer instances.
type PeerNotifier interface {
	// NotifyPeers sends a notification to all configured peers.
	NotifyPeers(ctx context.Context)
}
