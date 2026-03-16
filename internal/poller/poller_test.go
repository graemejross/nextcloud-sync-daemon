package poller

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPollerSendsEvents(t *testing.T) {
	p := New(50*time.Millisecond, quietLogger())
	trigger := make(chan daemon.Event, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.Start(ctx, trigger)
	}()

	// Wait for context to expire
	<-ctx.Done()
	err := <-done
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have received at least 2 events in 200ms with 50ms interval
	count := len(trigger)
	if count < 2 {
		t.Errorf("received %d events, want >= 2", count)
	}

	// Verify event fields
	event := <-trigger
	if event.Source != "poller" {
		t.Errorf("source = %q, want %q", event.Source, "poller")
	}
	if event.Time.IsZero() {
		t.Error("event time should not be zero")
	}
	if event.Path != "" {
		t.Errorf("path = %q, want empty", event.Path)
	}
}

func TestPollerDropsWhenFull(t *testing.T) {
	p := New(50*time.Millisecond, quietLogger())
	// Capacity 1 — second event should be dropped, not block
	trigger := make(chan daemon.Event, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.Start(ctx, trigger)
	}()

	<-ctx.Done()
	err := <-done
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Channel should have exactly 1 event (capacity 1, rest dropped)
	if len(trigger) != 1 {
		t.Errorf("trigger channel len = %d, want 1", len(trigger))
	}
}

func TestPollerStopsOnCancel(t *testing.T) {
	p := New(1*time.Hour, quietLogger()) // long interval — won't tick
	trigger := make(chan daemon.Event, 1)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Start(ctx, trigger)
	}()

	// Cancel immediately
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop within 2 seconds")
	}
}

func TestPollerName(t *testing.T) {
	p := New(time.Second, quietLogger())
	if p.Name() != "poller" {
		t.Errorf("name = %q, want %q", p.Name(), "poller")
	}
}
