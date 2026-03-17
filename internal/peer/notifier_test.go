package peer

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNotifyPeersSendsCorrectHeaders(t *testing.T) {
	var gotSecret string
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-Webhook-Secret")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New([]config.PeerConfig{
		{URL: srv.URL, Secret: "my-secret"},
	}, quietLogger(), nil)

	n.NotifyPeers(context.Background())

	if gotSecret != "my-secret" {
		t.Errorf("X-Webhook-Secret = %q, want %q", gotSecret, "my-secret")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestNotifyPeersSendsCorrectBody(t *testing.T) {
	var gotBody notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New([]config.PeerConfig{
		{URL: srv.URL, Secret: "s"},
	}, quietLogger(), nil)

	n.NotifyPeers(context.Background())

	if gotBody.Source != "peer" {
		t.Errorf("source = %q, want %q", gotBody.Source, "peer")
	}
	if gotBody.Time == "" {
		t.Error("time should not be empty")
	}
	// Verify time is valid RFC3339
	if _, err := time.Parse(time.RFC3339, gotBody.Time); err != nil {
		t.Errorf("time %q is not valid RFC3339: %v", gotBody.Time, err)
	}
}

func TestNotifyPeersMultiple(t *testing.T) {
	var count atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	srv1 := httptest.NewServer(handler)
	defer srv1.Close()
	srv2 := httptest.NewServer(handler)
	defer srv2.Close()

	n := New([]config.PeerConfig{
		{URL: srv1.URL, Secret: "s1"},
		{URL: srv2.URL, Secret: "s2"},
	}, quietLogger(), nil)

	n.NotifyPeers(context.Background())

	if got := count.Load(); got != 2 {
		t.Errorf("notification count = %d, want 2", got)
	}
}

func TestNotifyPeersFailureDoesNotPanic(t *testing.T) {
	// Peer that returns errors
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New([]config.PeerConfig{
		{URL: srv.URL, Secret: "s"},
		{URL: "http://127.0.0.1:1", Secret: "s"}, // connection refused
	}, quietLogger(), nil)

	// Should not panic
	n.NotifyPeers(context.Background())
}

func TestNotifyPeersNoPeers(t *testing.T) {
	n := New(nil, quietLogger(), nil)
	// Should return immediately without error
	n.NotifyPeers(context.Background())
}

func TestNotifyPeersTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer srv.Close()

	n := New([]config.PeerConfig{
		{URL: srv.URL, Secret: "s"},
	}, quietLogger(), nil)
	// Override timeout for test
	n.client.Timeout = 100 * time.Millisecond

	start := time.Now()
	n.NotifyPeers(context.Background())
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("NotifyPeers took %v, expected ~100ms timeout", elapsed)
	}
}
