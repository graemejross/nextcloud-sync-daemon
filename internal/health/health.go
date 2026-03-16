// Package health provides a health check endpoint that reports daemon status.
package health

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

// Status tracks the daemon's health state. All methods are safe for concurrent use.
type Status struct {
	mu        sync.RWMutex
	started   time.Time
	lastSync  *daemon.SyncResult
	syncCount int64
	failCount int64
	sources   map[string]bool
}

// NewStatus creates a Status with the current time as the start time.
func NewStatus() *Status {
	return &Status{
		started: time.Now(),
		sources: make(map[string]bool),
	}
}

// RecordSync records the result of a sync execution.
func (s *Status) RecordSync(result *daemon.SyncResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSync = result
	if result.ExitCode != 0 || result.Error != nil {
		s.failCount++
	} else {
		s.syncCount++
	}
}

// SetSourceRunning updates the running state of an event source.
func (s *Status) SetSourceRunning(name string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[name] = running
}

// response is the JSON structure returned by the health endpoint.
type response struct {
	Status           string          `json:"status"`
	Uptime           string          `json:"uptime"`
	LastSync         *string         `json:"last_sync"`
	LastSyncDuration *int64          `json:"last_sync_duration_ms"`
	LastSyncTrigger  *string         `json:"last_sync_trigger"`
	SyncCount        int64           `json:"sync_count"`
	FailCount        int64           `json:"fail_count"`
	Sources          map[string]bool `json:"sources"`
}

// Handler returns an http.HandlerFunc that serves the health check JSON response.
func (s *Status) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		resp := response{
			Status:    s.computeStatus(),
			Uptime:    time.Since(s.started).Truncate(time.Second).String(),
			SyncCount: s.syncCount,
			FailCount: s.failCount,
			Sources:   s.sources,
		}

		if s.lastSync != nil {
			ts := s.lastSync.StartTime.UTC().Format(time.RFC3339)
			resp.LastSync = &ts
			dur := s.lastSync.Duration.Milliseconds()
			resp.LastSyncDuration = &dur
			resp.LastSyncTrigger = &s.lastSync.Trigger
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// computeStatus returns "ok" or "degraded". Must be called with mu held.
func (s *Status) computeStatus() string {
	if s.lastSync != nil && (s.lastSync.ExitCode != 0 || s.lastSync.Error != nil) {
		return "degraded"
	}
	return "ok"
}
