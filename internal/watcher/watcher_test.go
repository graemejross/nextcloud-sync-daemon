package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestWatcherDetectsFileCreate(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 100*time.Millisecond, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	// Give the watcher time to set up
	time.Sleep(50 * time.Millisecond)

	// Create a file
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + margin
	select {
	case event := <-trigger:
		if event.Source != "watcher" {
			t.Errorf("source = %q, want %q", event.Source, "watcher")
		}
		if event.Time.IsZero() {
			t.Error("event time should not be zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within 2 seconds")
	}

	cancel()
	<-done
}

func TestWatcherDebounce(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 200*time.Millisecond, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	time.Sleep(50 * time.Millisecond)

	// Create multiple files rapidly — should debounce to one event
	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(name, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for debounce to fire
	select {
	case <-trigger:
		// good, got the debounced event
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within 2 seconds")
	}

	// There should not be another event quickly (debounce should have coalesced)
	select {
	case <-trigger:
		t.Error("received unexpected second event — debounce should have coalesced")
	case <-time.After(300 * time.Millisecond):
		// good, no second event
	}

	cancel()
	<-done
}

func TestWatcherExcludePattern(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 100*time.Millisecond, []string{`\.sync_.*\.db`}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	time.Sleep(50 * time.Millisecond)

	// Create an excluded file — should not trigger
	if err := os.WriteFile(filepath.Join(dir, ".sync_abc123.db"), []byte("sync"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait past debounce — should get no event
	select {
	case <-trigger:
		t.Error("received event for excluded file")
	case <-time.After(300 * time.Millisecond):
		// good, excluded
	}

	// Now create a non-excluded file — should trigger
	if err := os.WriteFile(filepath.Join(dir, "document.txt"), []byte("real"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-trigger:
		if event.Source != "watcher" {
			t.Errorf("source = %q, want %q", event.Source, "watcher")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event for non-excluded file")
	}

	cancel()
	<-done
}

func TestWatcherNewSubdirectory(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 100*time.Millisecond, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	time.Sleep(50 * time.Millisecond)

	// Create a new subdirectory
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	// Drain the event from directory creation
	select {
	case <-trigger:
	case <-time.After(2 * time.Second):
		t.Fatal("no event for directory creation")
	}

	// Give the watcher time to add the new directory watch
	time.Sleep(100 * time.Millisecond)

	// Create a file in the new subdirectory — should be watched
	if err := os.WriteFile(filepath.Join(subdir, "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-trigger:
		if event.Source != "watcher" {
			t.Errorf("source = %q, want %q", event.Source, "watcher")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event for file in new subdirectory — recursive watch may have failed")
	}

	cancel()
	<-done
}

func TestWatcherStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, time.Second, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	trigger := make(chan daemon.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop within 2 seconds")
	}
}

func TestWatcherDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 50*time.Millisecond, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Capacity 1 — simulates the engine's channel
	trigger := make(chan daemon.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx, trigger)
	}()

	time.Sleep(50 * time.Millisecond)

	// Fill the trigger channel first
	trigger <- daemon.Event{Source: "blocker"}

	// Create a file — debounce will fire but channel is full, should drop
	if err := os.WriteFile(filepath.Join(dir, "blocked.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce
	time.Sleep(200 * time.Millisecond)

	// Channel should still have exactly 1 event (the blocker)
	if len(trigger) != 1 {
		t.Errorf("trigger channel len = %d, want 1", len(trigger))
	}

	cancel()
	<-done
}

func TestWatcherInvalidExcludePattern(t *testing.T) {
	_, err := New(t.TempDir(), time.Second, []string{"[invalid"}, quietLogger())
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

func TestWatcherName(t *testing.T) {
	w, err := New(t.TempDir(), time.Second, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if w.Name() != "watcher" {
		t.Errorf("name = %q, want %q", w.Name(), "watcher")
	}
}

func TestIsExcluded(t *testing.T) {
	w, err := New(t.TempDir(), time.Second, []string{`\.sync_.*\.db`, `\.tmp$`}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path     string
		excluded bool
	}{
		{".sync_abc123.db", true},
		{".sync_xyz.db", true},
		{"document.txt", false},
		{"notes.tmp", true},
		{"subdir/.sync_test.db", true},
		{"subdir/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := w.isExcluded(tt.path); got != tt.excluded {
				t.Errorf("isExcluded(%q) = %v, want %v", tt.path, got, tt.excluded)
			}
		})
	}
}

