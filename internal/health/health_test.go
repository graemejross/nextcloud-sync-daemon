package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func TestNewStatus(t *testing.T) {
	before := time.Now()
	s := NewStatus()
	after := time.Now()

	if s.started.Before(before) || s.started.After(after) {
		t.Errorf("started = %v, want between %v and %v", s.started, before, after)
	}
	if s.sources == nil {
		t.Error("sources map is nil")
	}
}

func TestRecordSyncSuccess(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{
		StartTime: time.Now(),
		Duration:  100 * time.Millisecond,
		ExitCode:  0,
		Trigger:   "poller",
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.syncCount != 1 {
		t.Errorf("syncCount = %d, want 1", s.syncCount)
	}
	if s.failCount != 0 {
		t.Errorf("failCount = %d, want 0", s.failCount)
	}
}

func TestRecordSyncFailureExitCode(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{
		StartTime: time.Now(),
		Duration:  50 * time.Millisecond,
		ExitCode:  1,
		Trigger:   "watcher",
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.syncCount != 0 {
		t.Errorf("syncCount = %d, want 0", s.syncCount)
	}
	if s.failCount != 1 {
		t.Errorf("failCount = %d, want 1", s.failCount)
	}
}

func TestRecordSyncFailureError(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{
		StartTime: time.Now(),
		Duration:  0,
		ExitCode:  0,
		Error:     fmt.Errorf("exec failed"),
		Trigger:   "webhook",
	})

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failCount != 1 {
		t.Errorf("failCount = %d, want 1", s.failCount)
	}
}

func TestSetSourceRunning(t *testing.T) {
	s := NewStatus()
	s.SetSourceRunning("watcher", true)
	s.SetSourceRunning("poller", true)
	s.SetSourceRunning("webhook", false)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.sources["watcher"] {
		t.Error("watcher should be running")
	}
	if !s.sources["poller"] {
		t.Error("poller should be running")
	}
	if s.sources["webhook"] {
		t.Error("webhook should not be running")
	}
}

func TestHandlerContentType(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandlerJSONStructure(t *testing.T) {
	s := NewStatus()
	s.SetSourceRunning("watcher", true)
	s.SetSourceRunning("poller", true)
	s.RecordSync(&daemon.SyncResult{
		StartTime: time.Date(2026, 3, 16, 10, 30, 0, 0, time.UTC),
		Duration:  1234 * time.Millisecond,
		ExitCode:  0,
		Trigger:   "webhook",
	})

	handler := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Uptime == "" {
		t.Error("uptime is empty")
	}
	if resp.LastSync == nil || *resp.LastSync != "2026-03-16T10:30:00Z" {
		t.Errorf("last_sync = %v, want 2026-03-16T10:30:00Z", resp.LastSync)
	}
	if resp.LastSyncDuration == nil || *resp.LastSyncDuration != 1234 {
		t.Errorf("last_sync_duration_ms = %v, want 1234", resp.LastSyncDuration)
	}
	if resp.LastSyncTrigger == nil || *resp.LastSyncTrigger != "webhook" {
		t.Errorf("last_sync_trigger = %v, want webhook", resp.LastSyncTrigger)
	}
	if resp.SyncCount != 1 {
		t.Errorf("sync_count = %d, want 1", resp.SyncCount)
	}
	if resp.FailCount != 0 {
		t.Errorf("fail_count = %d, want 0", resp.FailCount)
	}
	if !resp.Sources["watcher"] {
		t.Error("sources.watcher should be true")
	}
	if !resp.Sources["poller"] {
		t.Error("sources.poller should be true")
	}
}

func TestHandlerNoSyncsYet(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok (no syncs = ok)", resp.Status)
	}
	if resp.LastSync != nil {
		t.Errorf("last_sync = %v, want nil", resp.LastSync)
	}
	if resp.LastSyncDuration != nil {
		t.Errorf("last_sync_duration_ms = %v, want nil", resp.LastSyncDuration)
	}
	if resp.LastSyncTrigger != nil {
		t.Errorf("last_sync_trigger = %v, want nil", resp.LastSyncTrigger)
	}
}

func TestStatusTransitions(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	getStatus := func() string {
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		var resp response
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		return resp.Status
	}

	// Initially ok
	if got := getStatus(); got != "ok" {
		t.Errorf("initial status = %q, want ok", got)
	}

	// After success → ok
	s.RecordSync(&daemon.SyncResult{ExitCode: 0, Trigger: "poller"})
	if got := getStatus(); got != "ok" {
		t.Errorf("after success status = %q, want ok", got)
	}

	// After failure → degraded
	s.RecordSync(&daemon.SyncResult{ExitCode: 1, Trigger: "poller"})
	if got := getStatus(); got != "degraded" {
		t.Errorf("after failure status = %q, want degraded", got)
	}

	// After recovery → ok
	s.RecordSync(&daemon.SyncResult{ExitCode: 0, Trigger: "poller"})
	if got := getStatus(); got != "ok" {
		t.Errorf("after recovery status = %q, want ok", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	var wg sync.WaitGroup
	const goroutines = 50

	// Hammer RecordSync from multiple goroutines
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			exitCode := 0
			if n%3 == 0 {
				exitCode = 1
			}
			s.RecordSync(&daemon.SyncResult{
				StartTime: time.Now(),
				Duration:  time.Duration(n) * time.Millisecond,
				ExitCode:  exitCode,
				Trigger:   "test",
			})
		}(i)
	}

	// Hammer SetSourceRunning
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.SetSourceRunning(fmt.Sprintf("source-%d", n%5), n%2 == 0)
		}(i)
	}

	// Hammer Handler reads
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("handler returned %d during concurrent access", rec.Code)
			}
		}()
	}

	wg.Wait()

	// Verify counts are consistent
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := s.syncCount + s.failCount
	if total != goroutines {
		t.Errorf("total syncs = %d, want %d", total, goroutines)
	}
}
