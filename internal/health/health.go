// Package health provides a health check endpoint that reports daemon status.
package health

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/scan"
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
	invalidSeen         map[string]bool
	conflictFiles       []conflictEntry
	conflictCount       int
	conflictSeen        map[string]bool
}

// NewStatus creates a Status with the current time as the start time.
func NewStatus() *Status {
	return &Status{
		started:       time.Now(),
		sources:       make(map[string]bool),
		triggerCounts: make(map[string]int64),
		invalidSeen:   make(map[string]bool),
		conflictSeen:  make(map[string]bool),
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

	// Remember every path the scan saw, not just the ones the endpoint will
	// show. Otherwise a file beyond the display cap gets counted a second time
	// when the watcher meets it.
	s.invalidSeen = make(map[string]bool, len(paths))
	for _, p := range paths {
		s.invalidSeen[p] = true
	}

	s.invalidNames = append([]string(nil), paths...)
	if len(s.invalidNames) > maxDisplayedNames {
		s.invalidNames = s.invalidNames[:maxDisplayedNames]
	}
	s.invalidNameCount = total
}

// maxDisplayedNames bounds how many offending paths the health response lists.
// The count beside them is not bounded.
const maxDisplayedNames = 50

// AddInvalidName records one further offending path, for names that appear
// after the startup scan. Repeats are ignored so a file touched repeatedly is
// counted once.
func (s *Status) AddInvalidName(path string, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidSeen[path] {
		return
	}
	if s.invalidSeen == nil {
		s.invalidSeen = make(map[string]bool)
	}
	s.invalidSeen[path] = true

	s.invalidNameCount++
	if len(s.invalidNames) < max {
		s.invalidNames = append(s.invalidNames, path)
	}
}

// conflictEntry is one conflicted copy as the endpoint reports it.
type conflictEntry struct {
	Path     string `json:"path"`
	Modified string `json:"modified"`
}

// maxDisplayedConflicts bounds how many conflicted copies the health response
// lists. The count beside them is not bounded.
const maxDisplayedConflicts = 10

// SetConflictFiles records the conflicted copies a scan found, newest first
// (Refs #47).
func (s *Status) SetConflictFiles(conflicts []scan.Conflict, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.conflictSeen = make(map[string]bool, len(conflicts))
	s.conflictFiles = nil
	for _, c := range conflicts {
		s.conflictSeen[c.Path] = true
		if len(s.conflictFiles) < maxDisplayedConflicts {
			s.conflictFiles = append(s.conflictFiles, conflictEntry{
				Path:     c.Path,
				Modified: c.ModTime.UTC().Format(time.RFC3339),
			})
		}
	}
	s.conflictCount = total
}

// AddConflictFile records one conflicted copy seen after the startup scan.
// Repeats are ignored, so a file touched several times is counted once.
func (s *Status) AddConflictFile(path string, modTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflictSeen[path] {
		return
	}
	if s.conflictSeen == nil {
		s.conflictSeen = make(map[string]bool)
	}
	s.conflictSeen[path] = true

	s.conflictCount++

	// Keep the list newest first, the order the scan established, so a
	// conflict written just now sorts above one from three years ago instead
	// of landing at the bottom.
	s.conflictFiles = append(s.conflictFiles, conflictEntry{
		Path:     path,
		Modified: modTime.UTC().Format(time.RFC3339),
	})
	sort.SliceStable(s.conflictFiles, func(i, j int) bool {
		return s.conflictFiles[i].Modified > s.conflictFiles[j].Modified
	})
	if len(s.conflictFiles) > maxDisplayedConflicts {
		s.conflictFiles = s.conflictFiles[:maxDisplayedConflicts]
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

	// Conflicted copies the sync client has left in the tree (Refs #47).
	// ConflictFileCount is exact; ConflictFiles lists the newest few.
	ConflictFileCount int             `json:"conflict_file_count,omitempty"`
	ConflictFiles     []conflictEntry `json:"conflict_files,omitempty"`
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

		if s.conflictCount > 0 {
			resp.ConflictFileCount = s.conflictCount
			resp.ConflictFiles = append([]conflictEntry(nil), s.conflictFiles...)
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
