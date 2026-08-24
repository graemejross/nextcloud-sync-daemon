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
	mu                  sync.RWMutex
	started             time.Time
	lastSync            *daemon.SyncResult
	syncCount           int64
	failCount           int64
	sources             map[string]bool
	triggerCounts       map[string]int64
	lastWebhookReceived *time.Time
	invalidNames        []string
	invalidNameCount    int
}

// NewStatus creates a Status with the current time as the start time.
func NewStatus() *Status {
	return &Status{
		started:       time.Now(),
		sources:       make(map[string]bool),
		triggerCounts: make(map[string]int64),
	}
}

// RecordSync records the result of a sync execution.
func (s *Status) RecordSync(result *daemon.SyncResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSync = result
	if result.Trigger != "" {
		s.triggerCounts[result.Trigger]++
	}
	if result.ExitCode != 0 || result.Error != nil {
		s.failCount++
	} else {
		s.syncCount++
	}
}

// SetInvalidNames records the result of a scan for filenames the server will
// reject (Refs #45). Paths are already escaped for display.
func (s *Status) SetInvalidNames(paths []string, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidNames = append([]string(nil), paths...)
	s.invalidNameCount = total
}

// AddInvalidName records one further offending path, for names that appear
// after the startup scan. Repeats are ignored so a file touched repeatedly is
// counted once.
func (s *Status) AddInvalidName(path string, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.invalidNames {
		if existing == path {
			return
		}
	}
	s.invalidNameCount++
	if len(s.invalidNames) < max {
		s.invalidNames = append(s.invalidNames, path)
	}
}

// RecordWebhookReceived records the time a valid webhook was received.
func (s *Status) RecordWebhookReceived(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWebhookReceived = &t
}

// SetSourceRunning updates the running state of an event source.
func (s *Status) SetSourceRunning(name string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[name] = running
}

// response is the JSON structure returned by the health endpoint.
type response struct {
	Status              string           `json:"status"`
	Uptime              string           `json:"uptime"`
	LastSync            *string          `json:"last_sync"`
	LastSyncDuration    *int64           `json:"last_sync_duration_ms"`
	LastSyncTrigger     *string          `json:"last_sync_trigger"`
	SyncCount           int64            `json:"sync_count"`
	FailCount           int64            `json:"fail_count"`
	Sources             map[string]bool  `json:"sources"`
	TriggerCounts       map[string]int64 `json:"trigger_counts"`
	LastWebhookReceived *string          `json:"last_webhook_received"`

	// Failure detail for the last sync, present only while that sync is the
	// failed one (Refs #44). A monitor seeing "degraded" gets the offending
	// item here instead of having to read the journal.
	LastFailReason *string `json:"last_fail_reason,omitempty"`
	LastFailPath   *string `json:"last_fail_path,omitempty"`
	LastFailDetail *string `json:"last_fail_detail,omitempty"`

	// Local filenames the server will refuse (Refs #45). Present only when the
	// daemon has found some; InvalidNames is capped, InvalidNameCount is not.
	InvalidNameCount int      `json:"invalid_name_count,omitempty"`
	InvalidNames     []string `json:"invalid_names,omitempty"`
}

// Handler returns an http.HandlerFunc that serves the health check JSON response.
func (s *Status) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		// Copy triggerCounts under lock to avoid exposing internal map
		counts := make(map[string]int64, len(s.triggerCounts))
		for k, v := range s.triggerCounts {
			counts[k] = v
		}

		resp := response{
			Status:        s.computeStatus(),
			Uptime:        time.Since(s.started).Truncate(time.Second).String(),
			SyncCount:     s.syncCount,
			FailCount:     s.failCount,
			Sources:       s.sources,
			TriggerCounts: counts,
		}

		if s.lastSync != nil {
			ts := s.lastSync.StartTime.UTC().Format(time.RFC3339)
			resp.LastSync = &ts
			dur := s.lastSync.Duration.Milliseconds()
			resp.LastSyncDuration = &dur
			resp.LastSyncTrigger = &s.lastSync.Trigger

			if s.lastSync.FailReason != "" {
				resp.LastFailReason = &s.lastSync.FailReason
			}
			if s.lastSync.FailPath != "" {
				resp.LastFailPath = &s.lastSync.FailPath
			}
			if s.lastSync.FailDetail != "" {
				resp.LastFailDetail = &s.lastSync.FailDetail
			}
		}

		if s.invalidNameCount > 0 {
			resp.InvalidNameCount = s.invalidNameCount
			resp.InvalidNames = append([]string(nil), s.invalidNames...)
		}

		if s.lastWebhookReceived != nil {
			ts := s.lastWebhookReceived.UTC().Format(time.RFC3339)
			resp.LastWebhookReceived = &ts
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
