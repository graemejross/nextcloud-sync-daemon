package engine

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeExecutor counts calls and optionally sleeps.
type fakeExecutor struct {
	calls    atomic.Int64
	delay    time.Duration
	exitCode int
}

func (f *fakeExecutor) Run(ctx context.Context) (*daemon.SyncResult, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return &daemon.SyncResult{ExitCode: -1, Error: ctx.Err()}, ctx.Err()
		}
	}
	return &daemon.SyncResult{
		StartTime: time.Now(),
		Duration:  f.delay,
		ExitCode:  f.exitCode,
	}, nil
}

// fakeSource sends events on demand via a channel, or auto-sends at a rate.
type fakeSource struct {
	name     string
	events   int
	interval time.Duration
}

func (f *fakeSource) Start(ctx context.Context, trigger chan<- daemon.Event) error {
	sent := 0
	for {
		if f.events > 0 && sent >= f.events {
			// Wait for cancellation
			<-ctx.Done()
			return nil
		}

		if f.interval > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(f.interval):
			}
		}

		event := daemon.Event{Source: f.name, Time: time.Now()}
		select {
		case trigger <- event:
			sent++
		default:
			sent++ // count dropped events too
		}

		if f.events > 0 && sent >= f.events {
			<-ctx.Done()
			return nil
		}
	}
}

func (f *fakeSource) Name() string { return f.name }

func TestEngineSingleSync(t *testing.T) {
	exec := &fakeExecutor{}
	src := &fakeSource{name: "test", events: 1, interval: 10 * time.Millisecond}

	eng := New(exec, 0, quietLogger(), src)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	eng.Run(ctx)

	if calls := exec.calls.Load(); calls < 1 {
		t.Errorf("executor calls = %d, want >= 1", calls)
	}
}

func TestEngineCoalescing(t *testing.T) {
	// Send many events rapidly — should result in far fewer syncs than events
	exec := &fakeExecutor{delay: 50 * time.Millisecond}
	src := &fakeSource{name: "burst", events: 20, interval: 5 * time.Millisecond}

	eng := New(exec, 0, quietLogger(), src)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	eng.Run(ctx)

	calls := exec.calls.Load()
	// With 20 events at 5ms intervals and 50ms sync time, the capacity-1 channel
	// means most events are dropped. We should see significantly fewer syncs than events.
	if calls >= 20 {
		t.Errorf("executor calls = %d, expected coalescing to reduce below 20", calls)
	}
	if calls < 1 {
		t.Errorf("executor calls = %d, want >= 1", calls)
	}
}

func TestEngineCooldown(t *testing.T) {
	exec := &fakeExecutor{}
	// Send events every 10ms
	src := &fakeSource{name: "rapid", events: 0, interval: 10 * time.Millisecond}

	// 200ms cooldown — should severely limit syncs
	eng := New(exec, 200*time.Millisecond, quietLogger(), src)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	eng.Run(ctx)

	calls := exec.calls.Load()
	// In 500ms with 200ms cooldown: first sync at ~10ms, next allowed at ~210ms, next at ~410ms
	// So max ~3 syncs
	if calls > 4 {
		t.Errorf("executor calls = %d, want <= 4 with 200ms cooldown in 500ms", calls)
	}
	if calls < 1 {
		t.Errorf("executor calls = %d, want >= 1", calls)
	}
}

func TestEngineGracefulShutdown(t *testing.T) {
	exec := &fakeExecutor{delay: 50 * time.Millisecond}
	src := &fakeSource{name: "test", events: 0, interval: 20 * time.Millisecond}

	eng := New(exec, 0, quietLogger(), src)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- eng.Run(ctx)
	}()

	// Let it run for a bit
	time.Sleep(100 * time.Millisecond)

	// Cancel and verify it shuts down promptly
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not shut down within 5 seconds")
	}
}

func TestEngineMultipleSources(t *testing.T) {
	exec := &fakeExecutor{}
	src1 := &fakeSource{name: "source1", events: 1, interval: 10 * time.Millisecond}
	src2 := &fakeSource{name: "source2", events: 1, interval: 20 * time.Millisecond}

	eng := New(exec, 0, quietLogger(), src1, src2)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	eng.Run(ctx)

	if calls := exec.calls.Load(); calls < 2 {
		t.Errorf("executor calls = %d, want >= 2 (one per source)", calls)
	}
}

func TestEngineNoSources(t *testing.T) {
	exec := &fakeExecutor{}
	eng := New(exec, 0, quietLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := exec.calls.Load(); calls != 0 {
		t.Errorf("executor calls = %d, want 0 (no sources)", calls)
	}
}

func TestEngineNonZeroExitContinues(t *testing.T) {
	exec := &fakeExecutor{exitCode: 1}
	src := &fakeSource{name: "test", events: 0, interval: 20 * time.Millisecond}

	eng := New(exec, 0, quietLogger(), src)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	eng.Run(ctx)

	// Engine should continue after non-zero exit, not stop
	if calls := exec.calls.Load(); calls < 2 {
		t.Errorf("executor calls = %d, want >= 2 (should continue after failure)", calls)
	}
}
