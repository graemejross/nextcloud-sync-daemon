package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/scan"
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

func TestTriggerCounts(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{ExitCode: 0, Trigger: "watcher"})
	s.RecordSync(&daemon.SyncResult{ExitCode: 0, Trigger: "watcher"})
	s.RecordSync(&daemon.SyncResult{ExitCode: 0, Trigger: "webhook"})
	s.RecordSync(&daemon.SyncResult{ExitCode: 1, Trigger: "poller"})

	handler := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TriggerCounts["watcher"] != 2 {
		t.Errorf("trigger_counts.watcher = %d, want 2", resp.TriggerCounts["watcher"])
	}
	if resp.TriggerCounts["webhook"] != 1 {
		t.Errorf("trigger_counts.webhook = %d, want 1", resp.TriggerCounts["webhook"])
	}
	if resp.TriggerCounts["poller"] != 1 {
		t.Errorf("trigger_counts.poller = %d, want 1", resp.TriggerCounts["poller"])
	}
}

func TestTriggerCountsNoSyncs(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TriggerCounts == nil {
		t.Error("trigger_counts should be empty map, not nil")
	}
	if len(resp.TriggerCounts) != 0 {
		t.Errorf("trigger_counts length = %d, want 0", len(resp.TriggerCounts))
	}
}

func TestRecordWebhookReceived(t *testing.T) {
	s := NewStatus()
	ts := time.Date(2026, 3, 17, 14, 30, 0, 0, time.UTC)
	s.RecordWebhookReceived(ts)

	handler := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.LastWebhookReceived == nil {
		t.Fatal("last_webhook_received should not be nil")
	}
	if *resp.LastWebhookReceived != "2026-03-17T14:30:00Z" {
		t.Errorf("last_webhook_received = %q, want 2026-03-17T14:30:00Z", *resp.LastWebhookReceived)
	}
}

func TestRecordWebhookReceivedNotSet(t *testing.T) {
	s := NewStatus()
	handler := s.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.LastWebhookReceived != nil {
		t.Errorf("last_webhook_received = %v, want nil", resp.LastWebhookReceived)
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

	// Hammer RecordWebhookReceived
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RecordWebhookReceived(time.Now())
		}()
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

// A degraded daemon must name the item that failed, so a monitor reading the
// endpoint does not have to go to the journal for it (Refs #44).
func TestHandlerExposesFailureDetail(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{
		StartTime:  time.Date(2026, 3, 16, 10, 30, 0, 0, time.UTC),
		Duration:   900 * time.Millisecond,
		ExitCode:   1,
		Trigger:    "poller",
		FailReason: "BlacklistedError",
		FailPath:   `Drawings/plan\r\n v2.pdf`,
		FailDetail: "400 Bad Request",
	})

	handler := s.Handler()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "degraded" {
		t.Errorf("status = %q, want degraded", resp.Status)
	}
	if resp.LastFailReason == nil || *resp.LastFailReason != "BlacklistedError" {
		t.Errorf("last_fail_reason = %v, want BlacklistedError", resp.LastFailReason)
	}
	if resp.LastFailPath == nil || *resp.LastFailPath != `Drawings/plan\r\n v2.pdf` {
		t.Errorf("last_fail_path = %v, want the escaped path", resp.LastFailPath)
	}
	if resp.LastFailDetail == nil || *resp.LastFailDetail != "400 Bad Request" {
		t.Errorf("last_fail_detail = %v, want 400 Bad Request", resp.LastFailDetail)
	}
}

// After a recovery the failure fields must disappear, so the endpoint always
// describes the current state rather than a stale one.
func TestHandlerDropsFailureDetailAfterRecovery(t *testing.T) {
	s := NewStatus()
	s.RecordSync(&daemon.SyncResult{
		StartTime:  time.Now(),
		ExitCode:   1,
		Trigger:    "poller",
		FailReason: "NormalError",
		FailPath:   "Reports/q1.csv",
	})
	s.RecordSync(&daemon.SyncResult{
		StartTime: time.Now(),
		ExitCode:  0,
		Trigger:   "poller",
	})

	handler := s.Handler()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/", nil))

	body := rec.Body.String()
	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.LastFailReason != nil || resp.LastFailPath != nil {
		t.Errorf("failure fields survived a successful sync: %v %v", resp.LastFailReason, resp.LastFailPath)
	}
	if strings.Contains(body, "last_fail_") {
		t.Errorf("failure keys present in JSON after recovery: %s", body)
	}
	if resp.FailCount != 1 {
		t.Errorf("fail_count = %d, want 1 (the counter still records the failure)", resp.FailCount)
	}
}

// Filenames the server will reject are reported with a count that is not
// capped and a list that is (Refs #45).
func TestInvalidNamesReported(t *testing.T) {
	s := NewStatus()
	s.SetInvalidNames([]string{`sub/Schematic\r\n v2.pdf`}, 1)

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.InvalidNameCount != 1 {
		t.Errorf("invalid_name_count = %d, want 1", resp.InvalidNameCount)
	}
	if len(resp.InvalidNames) != 1 || resp.InvalidNames[0] != `sub/Schematic\r\n v2.pdf` {
		t.Errorf("invalid_names = %q, want the escaped path", resp.InvalidNames)
	}
}

func TestAddInvalidNameDeduplicatesAndCaps(t *testing.T) {
	s := NewStatus()
	for i := 0; i < 3; i++ {
		s.AddInvalidName(`one\r.txt`, 2)
	}
	s.AddInvalidName(`two\r.txt`, 2)
	s.AddInvalidName(`three\r.txt`, 2)

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.InvalidNameCount != 3 {
		t.Errorf("invalid_name_count = %d, want 3 (one entry per distinct path)", resp.InvalidNameCount)
	}
	if len(resp.InvalidNames) != 2 {
		t.Errorf("invalid_names holds %d entries, want the cap of 2", len(resp.InvalidNames))
	}
}

// A clean tree leaves both fields out of the response entirely.
func TestInvalidNamesAbsentWhenClean(t *testing.T) {
	s := NewStatus()
	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	if body := rec.Body.String(); strings.Contains(body, "invalid_name") {
		t.Errorf("invalid-name keys present on a clean daemon: %s", body)
	}
}

// A path already counted by the startup scan must not be counted again when the
// watcher meets it, even when it falls outside the displayed list.
func TestAddInvalidNameDoesNotDoubleCountScannedPaths(t *testing.T) {
	s := NewStatus()

	var paths []string
	for i := 0; i < maxDisplayedNames+5; i++ {
		paths = append(paths, fmt.Sprintf("bad-%d\\r.txt", i))
	}
	s.SetInvalidNames(paths, len(paths))

	// One inside the displayed list, one outside it.
	s.AddInvalidName(paths[0], maxDisplayedNames)
	s.AddInvalidName(paths[len(paths)-1], maxDisplayedNames)

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.InvalidNameCount != len(paths) {
		t.Errorf("invalid_name_count = %d, want %d", resp.InvalidNameCount, len(paths))
	}
	if len(resp.InvalidNames) != maxDisplayedNames {
		t.Errorf("invalid_names holds %d entries, want the %d cap", len(resp.InvalidNames), maxDisplayedNames)
	}
}

// Conflicted copies are reported with an exact count and the newest few paths
// (Refs #47).
func TestConflictFilesReported(t *testing.T) {
	s := NewStatus()
	s.SetConflictFiles([]scan.Conflict{
		{Path: "report (conflicted copy 2026-04-10 191233).pdf", ModTime: time.Date(2026, 4, 10, 19, 12, 33, 0, time.UTC)},
		{Path: "notes (conflicted copy 2023-01-02 101010).md", ModTime: time.Date(2023, 1, 2, 10, 10, 10, 0, time.UTC)},
	}, 2)

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConflictFileCount != 2 {
		t.Errorf("conflict_file_count = %d, want 2", resp.ConflictFileCount)
	}
	if len(resp.ConflictFiles) != 2 {
		t.Fatalf("conflict_files holds %d entries, want 2", len(resp.ConflictFiles))
	}
	if resp.ConflictFiles[0].Modified != "2026-04-10T19:12:33Z" {
		t.Errorf("modified = %q, want the file's mtime in RFC3339", resp.ConflictFiles[0].Modified)
	}
}

// The list is capped, the count is not, and a path already counted by the scan
// is not counted again when the watcher sees it.
func TestConflictFilesCapAndDedup(t *testing.T) {
	s := NewStatus()

	var conflicts []scan.Conflict
	for i := 0; i < maxDisplayedConflicts+5; i++ {
		conflicts = append(conflicts, scan.Conflict{Path: fmt.Sprintf("f%d (conflicted copy 2026-04-10 191233).pdf", i)})
	}
	s.SetConflictFiles(conflicts, len(conflicts))

	s.AddConflictFile(conflicts[0].Path, time.Now())
	s.AddConflictFile(conflicts[len(conflicts)-1].Path, time.Now())
	s.AddConflictFile("new (conflicted copy 2026-08-24 120000).pdf", time.Now())

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := len(conflicts) + 1; resp.ConflictFileCount != want {
		t.Errorf("conflict_file_count = %d, want %d", resp.ConflictFileCount, want)
	}
	if len(resp.ConflictFiles) != maxDisplayedConflicts {
		t.Errorf("conflict_files holds %d entries, want the %d cap", len(resp.ConflictFiles), maxDisplayedConflicts)
	}
}

func TestConflictFilesAbsentWhenNone(t *testing.T) {
	s := NewStatus()
	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	if body := rec.Body.String(); strings.Contains(body, "conflict_file") {
		t.Errorf("conflict keys present when there are none: %s", body)
	}
}

// A conflict seen at runtime takes its place by date, not at the end of the
// list, so "newest first" holds however the entry arrived.
func TestAddConflictFileKeepsNewestFirst(t *testing.T) {
	s := NewStatus()
	s.SetConflictFiles([]scan.Conflict{
		{Path: "old (conflicted copy 2023-01-02 101010).pdf", ModTime: time.Date(2023, 1, 2, 10, 10, 10, 0, time.UTC)},
	}, 1)

	s.AddConflictFile("new (conflicted copy 2026-08-24 174500).pdf", time.Date(2026, 8, 24, 17, 45, 0, 0, time.UTC))

	rec := httptest.NewRecorder()
	s.Handler()(rec, httptest.NewRequest("GET", "/", nil))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ConflictFiles) != 2 {
		t.Fatalf("conflict_files holds %d entries, want 2", len(resp.ConflictFiles))
	}
	if !strings.HasPrefix(resp.ConflictFiles[0].Path, "new ") {
		t.Errorf("first entry is %q, want the 2026 one", resp.ConflictFiles[0].Path)
	}
}
