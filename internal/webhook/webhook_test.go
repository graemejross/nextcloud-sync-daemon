package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testServer(pathFilter string) (*Server, chan daemon.Event, http.HandlerFunc) {
	s := New("127.0.0.1:0", "test-secret", pathFilter, quietLogger())
	trigger := make(chan daemon.Event, 10)
	handler := s.handler(trigger)
	return s, trigger, handler
}

func TestWebhookValidPost(t *testing.T) {
	_, trigger, handler := testServer("/")

	body := `{"event":{"class":"NodeWrittenEvent","node":{"path":"/alice/documents/test.txt"}}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", w.Body.String(), "ok")
	}

	select {
	case event := <-trigger:
		if event.Source != "webhook" {
			t.Errorf("source = %q, want %q", event.Source, "webhook")
		}
		if event.Path != "/alice/documents/test.txt" {
			t.Errorf("path = %q, want %q", event.Path, "/alice/documents/test.txt")
		}
		if event.Time.IsZero() {
			t.Error("event time should not be zero")
		}
	default:
		t.Error("no event sent to trigger channel")
	}
}

func TestWebhookInvalidSecret(t *testing.T) {
	_, trigger, handler := testServer("/")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Webhook-Secret", "wrong-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	select {
	case <-trigger:
		t.Error("event should not be sent for invalid secret")
	default:
		// good
	}
}

func TestWebhookMissingSecret(t *testing.T) {
	_, trigger, handler := testServer("/")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	// No X-Webhook-Secret header
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	select {
	case <-trigger:
		t.Error("event should not be sent for missing secret")
	default:
	}
}

func TestWebhookGetHealthCheck(t *testing.T) {
	_, _, handler := testServer("/")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", w.Body.String(), "ok")
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	_, _, handler := testServer("/")

	req := httptest.NewRequest(http.MethodPut, "/", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhookPathFilterMatch(t *testing.T) {
	_, trigger, handler := testServer("/alice/")

	body := `{"event":{"class":"NodeCreatedEvent","node":{"path":"/alice/photos/holiday.jpg"}}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case event := <-trigger:
		if event.Path != "/alice/photos/holiday.jpg" {
			t.Errorf("path = %q, want %q", event.Path, "/alice/photos/holiday.jpg")
		}
	default:
		t.Error("matching path should trigger event")
	}
}

func TestWebhookPathFilterNoMatch(t *testing.T) {
	_, trigger, handler := testServer("/alice/")

	body := `{"event":{"class":"NodeWrittenEvent","node":{"path":"/bob/documents/secret.txt"}}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "filtered" {
		t.Errorf("body = %q, want %q", w.Body.String(), "filtered")
	}

	select {
	case <-trigger:
		t.Error("non-matching path should not trigger event")
	default:
		// good
	}
}

func TestWebhookMalformedJSON(t *testing.T) {
	_, trigger, handler := testServer("/")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json at all"))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	// Malformed JSON should still trigger a sync — we got a valid notification
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case event := <-trigger:
		if event.Path != "unknown" {
			t.Errorf("path = %q, want %q", event.Path, "unknown")
		}
	default:
		t.Error("malformed JSON should still trigger sync")
	}
}

func TestWebhookEmptyBody(t *testing.T) {
	_, trigger, handler := testServer("/")

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case event := <-trigger:
		if event.Path != "unknown" {
			t.Errorf("path = %q, want %q", event.Path, "unknown")
		}
	default:
		t.Error("empty body should still trigger sync")
	}
}

func TestWebhookDropsWhenFull(t *testing.T) {
	s := New("127.0.0.1:0", "test-secret", "/", quietLogger())
	trigger := make(chan daemon.Event, 1)
	handler := s.handler(trigger)

	// Pre-fill the channel
	trigger <- daemon.Event{Source: "blocker"}

	body := `{"event":{"class":"NodeWrittenEvent","node":{"path":"/test.txt"}}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Channel should still have exactly 1 event (the blocker)
	if len(trigger) != 1 {
		t.Errorf("trigger channel len = %d, want 1", len(trigger))
	}
}

func TestWebhookServerStartStop(t *testing.T) {
	s := New("127.0.0.1:0", "test-secret", "/", quietLogger())
	trigger := make(chan daemon.Event, 10)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx, trigger)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop within 5 seconds")
	}
}

func TestWebhookServerServes(t *testing.T) {
	// Use port 0 for auto-assignment — but Start() uses the configured listen address.
	// For this test, use a known free port.
	s := New("127.0.0.1:18767", "test-secret", "/", quietLogger())
	trigger := make(chan daemon.Event, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- s.Start(ctx, trigger)
	}()

	// Wait for server to be ready
	time.Sleep(200 * time.Millisecond)

	// Send a real HTTP request
	body := `{"event":{"class":"NodeCreatedEvent","node":{"path":"/alice/test.txt"}}}`
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18767/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case event := <-trigger:
		if event.Source != "webhook" {
			t.Errorf("source = %q, want %q", event.Source, "webhook")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}

	cancel()
	<-done
}

func TestExtractPath(t *testing.T) {
	s := New("", "", "/", quietLogger())

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"valid payload",
			`{"event":{"class":"NodeWrittenEvent","node":{"path":"/alice/doc.txt"}}}`,
			"/alice/doc.txt",
		},
		{
			"empty path",
			`{"event":{"class":"NodeDeletedEvent","node":{"path":""}}}`,
			"unknown",
		},
		{
			"missing node",
			`{"event":{"class":"NodeCreatedEvent"}}`,
			"unknown",
		},
		{
			"empty object",
			`{}`,
			"unknown",
		},
		{
			"invalid json",
			`not json`,
			"unknown",
		},
		{
			"empty body",
			``,
			"unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.extractPath([]byte(tt.body))
			if got != tt.want {
				t.Errorf("extractPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebhookRateLimiting(t *testing.T) {
	s := New("127.0.0.1:0", "test-secret", "/", quietLogger())
	s.rateMin = 100 * time.Millisecond // short interval for testing
	trigger := make(chan daemon.Event, 10)
	handler := s.handler(trigger)

	body := `{"event":{"class":"NodeWrittenEvent","node":{"path":"/test.txt"}}}`

	// First request should succeed
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Webhook-Secret", "test-secret")
	req.RemoteAddr = "192.168.1.1:12345"
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("first request: status = %d, want %d", w.Code, http.StatusOK)
	}

	// Second request from same IP should be rate-limited
	req2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req2.Header.Set("X-Webhook-Secret", "test-secret")
	req2.RemoteAddr = "192.168.1.1:12346" // same IP, different port
	w2 := httptest.NewRecorder()
	handler(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request: status = %d, want %d", w2.Code, http.StatusTooManyRequests)
	}

	// Request from different IP should succeed
	req3 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req3.Header.Set("X-Webhook-Secret", "test-secret")
	req3.RemoteAddr = "192.168.1.2:12345"
	w3 := httptest.NewRecorder()
	handler(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("different IP request: status = %d, want %d", w3.Code, http.StatusOK)
	}

	// After waiting, same IP should succeed again
	time.Sleep(150 * time.Millisecond)
	req4 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req4.Header.Set("X-Webhook-Secret", "test-secret")
	req4.RemoteAddr = "192.168.1.1:12347"
	w4 := httptest.NewRecorder()
	handler(w4, req4)
	if w4.Code != http.StatusOK {
		t.Errorf("after cooldown: status = %d, want %d", w4.Code, http.StatusOK)
	}
}

func TestWebhookName(t *testing.T) {
	s := New("", "", "/", quietLogger())
	if s.Name() != "webhook" {
		t.Errorf("name = %q, want %q", s.Name(), "webhook")
	}
}

// Keep fmt import
var _ = fmt.Sprint
