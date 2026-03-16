# Design Document: nextcloud-sync-daemon

> Refs #2

## Overview

A single Go binary that replaces the prototype's four components (filesystem watcher, webhook listener, sync script, systemd timer) with one event-driven daemon. It wraps `nextcloudcmd` — it does not reimplement the Nextcloud sync protocol.

## Package Structure

```
nextcloud-sync-daemon/
├── cmd/
│   └── nextcloud-sync-daemon/
│       └── main.go              # Entry point, signal handling, systemd notify
├── internal/
│   ├── config/
│   │   ├── config.go            # YAML parsing, validation, defaults
│   │   └── config_test.go
│   ├── engine/
│   │   ├── engine.go            # Unified event loop, deduplication, scheduling
│   │   └── engine_test.go
│   ├── watcher/
│   │   ├── watcher.go           # Filesystem watcher (fsnotify)
│   │   └── watcher_test.go
│   ├── webhook/
│   │   ├── webhook.go           # HTTP server for Nextcloud push events
│   │   └── webhook_test.go
│   ├── poller/
│   │   ├── poller.go            # Timer-based polling fallback
│   │   └── poller_test.go
│   ├── sync/
│   │   ├── executor.go          # Runs nextcloudcmd, captures output
│   │   └── executor_test.go
│   └── health/
│       ├── health.go            # Health check endpoint, status reporting
│       └── health_test.go
├── docs/
│   └── DESIGN.md                # This file
├── prototype/                   # Historical reference (read-only)
├── go.mod
├── go.sum
├── Makefile
├── .goreleaser.yml
└── README.md
```

All application packages are under `internal/` — they are not importable by external code. This is deliberate: the public contract is the binary and its config file, not a Go library.

## Key Interfaces

```go
// EventSource produces sync trigger events. Each trigger layer implements this.
type EventSource interface {
    // Start begins producing events. Blocks until ctx is cancelled.
    // Sends a signal on the trigger channel whenever a sync should run.
    Start(ctx context.Context, trigger chan<- Event) error
    // Name returns a human-readable name for logging (e.g., "watcher", "webhook", "poller").
    Name() string
}

// Event represents a sync trigger from any source.
type Event struct {
    Source    string    // "watcher", "webhook", "poller"
    Time      time.Time
    Path      string    // file path that changed (empty for poll events)
}

// SyncExecutor runs nextcloudcmd and reports the result.
type SyncExecutor interface {
    // Run executes a sync. Returns when the sync completes.
    Run(ctx context.Context) (*SyncResult, error)
}

// SyncResult captures the outcome of a single sync run.
type SyncResult struct {
    StartTime time.Time
    Duration  time.Duration
    ExitCode  int
    Trigger   string    // what caused this sync
    Error     error
}
```

## Event Flow

```
  ┌──────────┐   ┌──────────┐   ┌──────────┐
  │ Watcher  │   │ Webhook  │   │  Poller  │
  │(fsnotify)│   │ (HTTP)   │   │ (ticker) │
  └────┬─────┘   └────┬─────┘   └────┬─────┘
       │              │              │
       └──────────────┼──────────────┘
                      │
                      ▼
              ┌───────────────┐
              │    Engine     │
              │               │
              │ ┌───────────┐ │
              │ │ Event     │ │  Buffered channel, capacity 1
              │ │ Channel   │ │  (coalesces burst events)
              │ └─────┬─────┘ │
              │       │       │
              │ ┌─────▼─────┐ │
              │ │ Cooldown  │ │  Enforces minimum interval
              │ │ Timer     │ │  between syncs (configurable)
              │ └─────┬─────┘ │
              │       │       │
              │ ┌─────▼─────┐ │
              │ │   Sync    │ │  Calls nextcloudcmd,
              │ │ Executor  │ │  captures result
              │ └───────────┘ │
              └───────────────┘
```

### Deduplication Strategy

The sync storm problem from the prototype is solved by design:

1. **Coalescing channel** — The event channel has capacity 1. When a sync trigger arrives and the channel is full, it is dropped (a sync is already pending). This replaces the prototype's file-based debounce.

2. **Cooldown timer** — After a sync completes, the engine ignores all events for a configurable cooldown period (default 30s). This replaces the prototype's file-based cooldown timestamp.

3. **Single goroutine consumer** — Only one goroutine reads from the event channel and executes syncs. No lock files, no PID files, no race conditions.

The flow:
```
Event arrives → channel full? → yes: drop (sync already pending)
                              → no: enqueue
Engine loop  → event received → cooldown active? → yes: drop
                                                  → no: run sync, start cooldown
```

### Why a capacity-1 channel works

With capacity 1, the channel holds at most one pending event. Multiple rapid events (e.g., 50 inotify events from a bulk file copy) collapse into a single sync. The engine doesn't need to track or deduplicate individual file paths — `nextcloudcmd` handles the full bidirectional sync regardless of which specific file triggered it.

## Component Details

### Engine (`internal/engine/`)

The central coordinator. Owns the event channel, runs the main loop, enforces cooldown.

```go
type Engine struct {
    config    *config.Config
    executor  SyncExecutor
    sources   []EventSource
    events    chan Event
    health    *health.Status
    logger    *slog.Logger
}

func (e *Engine) Run(ctx context.Context) error {
    // 1. Start all event sources in goroutines
    // 2. Main loop: read events, enforce cooldown, execute sync
    // 3. On ctx cancel: stop sources, wait for in-flight sync, return
}
```

Lifecycle:
1. Parse config, create components
2. Start event sources (each in its own goroutine)
3. Signal systemd readiness (`sd_notify("READY=1")`)
4. Main loop until SIGTERM/SIGINT
5. Graceful shutdown: cancel context, wait for in-flight sync (with timeout)

### Watcher (`internal/watcher/`)

Wraps `fsnotify` to monitor the local sync directory.

- Watches recursively (fsnotify requires explicit per-directory watches — add new directories as they appear)
- Filters exclude patterns from config (e.g., `.sync_*.db`)
- Sends one Event per debounce window (default 10s) — uses a timer reset pattern: first event starts a timer; subsequent events reset it; timer fires → send event
- Events during cooldown are still debounced locally (the engine will drop them, but the watcher doesn't need to know about cooldown)

### Webhook (`internal/webhook/`)

HTTP server receiving Nextcloud `webhook_listeners` push events.

- Validates `X-Webhook-Secret` header
- Parses JSON payload, extracts file path
- Filters by configured path prefix
- Sends Event to engine channel
- Health check on GET (delegated to health package)
- Timeouts: read 10s, write 10s, idle 120s

### Poller (`internal/poller/`)

Simple — `time.Ticker` at the configured interval. Sends an Event on each tick. The simplest event source.

### Sync Executor (`internal/sync/`)

Runs `nextcloudcmd` as a subprocess.

```go
type Executor struct {
    config *config.Config
    logger *slog.Logger
    mu     sync.Mutex  // prevents overlapping runs (belt and braces)
}

func (e *Executor) Run(ctx context.Context) (*SyncResult, error) {
    // Build command: nextcloudcmd [extra_args] -u USER -p PASSWORD --path REMOTE LOCAL SERVER
    // Set ctx deadline for sync timeout
    // Capture stdout/stderr
    // Return SyncResult with exit code and duration
}
```

Credential handling:
- Username from config
- Password from `password_file` (read at sync time, not cached — allows rotation)
- Password file permissions checked on startup (warn if world-readable)

Output handling:
- At `info` level: log sync start, duration, exit code, trigger source
- At `debug` level: log nextcloudcmd stdout/stderr line by line
- Structured fields: `trigger`, `duration_ms`, `exit_code`

### Health (`internal/health/`)

Tracks daemon state for the health endpoint and systemd watchdog.

```go
type Status struct {
    mu            sync.RWMutex
    started       time.Time
    lastSync      *SyncResult
    syncCount     int64
    failCount     int64
    sources       map[string]bool  // which event sources are running
}
```

Health endpoint response (JSON):
```json
{
    "status": "ok",
    "uptime": "4h32m",
    "last_sync": "2026-03-16T10:30:00Z",
    "last_sync_duration_ms": 1234,
    "last_sync_trigger": "webhook",
    "sync_count": 847,
    "fail_count": 2,
    "sources": {
        "watcher": true,
        "webhook": true,
        "poller": true
    }
}
```

### Config (`internal/config/`)

YAML parsed with `gopkg.in/yaml.v3`. Validated on load — fail fast with clear error messages.

```go
type Config struct {
    Server   ServerConfig   `yaml:"server"`
    Sync     SyncConfig     `yaml:"sync"`
    Watch    WatchConfig    `yaml:"watch"`
    Webhook  WebhookConfig  `yaml:"webhook"`
    Poll     PollConfig     `yaml:"poll"`
    Logging  LogConfig      `yaml:"logging"`
    Health   HealthConfig   `yaml:"health"`
}
```

Defaults (applied if not specified):
```yaml
sync:
  nextcloudcmd: nextcloudcmd     # found via PATH
  extra_args: []
  timeout: 5m

watch:
  enabled: true
  debounce: 10s
  cooldown: 30s
  exclude: ['\.sync_.*\.db']

webhook:
  enabled: false                  # requires server-side setup
  listen: 0.0.0.0:8767
  path_filter: /

poll:
  enabled: true
  interval: 60s

logging:
  level: info
  format: text

health:
  enabled: true
  listen: 127.0.0.1:8768          # localhost only by default
```

Validation rules:
- `server.url` required, must be valid URL
- `server.username` required
- `server.password` or `server.password_file` required (not both)
- `sync.local_dir` required, must exist and be writable
- `sync.timeout` > 0
- `watch.debounce` > 0, `watch.cooldown` >= `watch.debounce`
- `webhook.secret` required if webhook enabled
- `poll.interval` > 0
- At least one event source must be enabled

Config file path resolution:
1. `--config` flag
2. `$NEXTCLOUD_SYNC_CONFIG` environment variable
3. `/etc/nextcloud-sync-daemon/config.yaml`
4. `~/.config/nextcloud-sync-daemon/config.yaml`

## Logging

Uses Go's `log/slog` (standard library, no dependency).

Levels:
- **error** — sync failures, config errors, unrecoverable problems
- **warn** — permission issues on password file, event source restarts, sync timeouts
- **info** — sync start/complete, source events (without path details), startup/shutdown
- **debug** — individual file events, nextcloudcmd output, HTTP request details, cooldown decisions

Format: `text` (human-readable, default) or `json` (machine-parseable). Both write to stderr for journal integration.

## Systemd Integration

```ini
[Unit]
Description=Nextcloud Sync Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/nextcloud-sync-daemon --config /etc/nextcloud-sync-daemon/config.yaml
Restart=on-failure
RestartSec=5
WatchdogSec=120

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/user/nextcloud
PrivateTmp=yes

[Install]
WantedBy=default.target
```

- **Type=notify**: Daemon signals readiness after config is validated and all sources are started.
- **WatchdogSec**: Daemon sends heartbeat every 60s (half the watchdog interval). systemd restarts it if heartbeats stop.
- **ReadWritePaths**: Only the sync directory needs write access.

The daemon ships an example unit file. Users create their own with the correct paths.

## Error Handling

| Failure | Behaviour |
|---------|-----------|
| Config file missing/invalid | Exit 1 with clear error message |
| `nextcloudcmd` not found | Exit 1 at startup |
| `nextcloudcmd` sync fails (non-zero exit) | Log error, continue. Next event or poll retries |
| `nextcloudcmd` times out | Kill process, log warning, continue |
| Webhook HTTP error | Log, return 500, continue |
| fsnotify error | Log error, attempt to re-establish watches |
| Password file unreadable | Log error, skip sync, retry on next event |
| Network unreachable | nextcloudcmd handles this; daemon logs the failure and retries on next trigger |

Principle: **never exit on transient failures**. The daemon runs until explicitly stopped. Only config/setup errors at startup cause an exit.

## Testing Strategy

### Unit Tests

Each package has tests alongside the code. Key test cases:

- **config**: Valid/invalid YAML, defaults applied, validation errors, password file handling
- **engine**: Event coalescing (send N events, expect 1 sync), cooldown enforcement, graceful shutdown
- **watcher**: Exclude pattern matching, debounce timing (with fake clock)
- **webhook**: Secret validation, path filtering, payload parsing, malformed requests
- **poller**: Interval fires correctly (with fake clock)
- **sync**: Command construction, timeout handling, exit code capture

### Interface-Based Testing

The `EventSource` and `SyncExecutor` interfaces enable testing without real filesystems or HTTP:

```go
// Test the engine with a fake executor
type fakeExecutor struct {
    calls    int
    duration time.Duration
}

func (f *fakeExecutor) Run(ctx context.Context) (*SyncResult, error) {
    f.calls++
    time.Sleep(f.duration)
    return &SyncResult{ExitCode: 0, Duration: f.duration}, nil
}
```

### Integration Tests

Run with `-tags integration` (excluded from normal `go test`):

- **Sync executor**: Requires `nextcloudcmd` to be installed. Tests command construction and output capture against a mock Nextcloud server or with `--dry-run` if available.
- **Watcher**: Creates a temp directory, writes files, verifies events arrive.
- **Webhook**: Starts the HTTP server, sends test payloads, verifies events.

### No Mocked Database or Network

The daemon has no database and minimal network (one HTTP server, one subprocess call). Integration tests use real filesystem operations and real HTTP requests to localhost. No mock frameworks needed.

## Dependencies

| Dependency | Purpose | Rationale |
|------------|---------|-----------|
| `github.com/fsnotify/fsnotify` | Filesystem watching | De facto standard, cross-platform, well-maintained |
| `gopkg.in/yaml.v3` | YAML config parsing | Standard Go YAML library |
| `github.com/coreos/go-systemd/v22` | sd_notify, watchdog | Official systemd Go bindings |

Everything else uses the Go standard library:
- `log/slog` for structured logging
- `net/http` for webhook server and health endpoint
- `os/exec` for running nextcloudcmd
- `context` for cancellation and timeouts
- `time` for timers and tickers

**Go version:** 1.22+ (for `log/slog` in stdlib, available since 1.21).

## Implementation Phases

### Phase 1: Foundation
**Goal:** Binary that loads config and runs nextcloudcmd once.

1. `go mod init`, directory structure
2. Config package — YAML parsing, validation, defaults
3. Sync executor — runs nextcloudcmd with config values
4. CLI entry point — `--config` flag, loads config, runs one sync, exits
5. Tests for config and executor

**Milestone:** `nextcloud-sync-daemon --config config.yaml --once` performs a single sync.

### Phase 2: Event Loop
**Goal:** Daemon that runs continuously with polling.

1. Engine — event channel, main loop, cooldown enforcement
2. Poller — timer-based event source
3. Graceful shutdown — signal handling, context cancellation
4. Logging — slog setup, level/format from config
5. Tests for engine and poller

**Milestone:** Daemon starts, syncs on poll interval, stops cleanly on SIGTERM.

### Phase 3: Filesystem Watcher
**Goal:** Local file changes trigger immediate sync.

1. Watcher — fsnotify integration, recursive watches, exclude patterns
2. Debounce — timer-reset pattern for batching events
3. Wire into engine as an EventSource
4. Tests for watcher

**Milestone:** Editing a file in the sync directory triggers a sync within debounce window.

### Phase 4: Webhook Listener
**Goal:** Server-side changes trigger immediate sync.

1. Webhook HTTP server — secret validation, payload parsing, path filtering
2. Wire into engine as an EventSource
3. Tests for webhook

**Milestone:** Nextcloud server push events trigger syncs.

### Phase 5: Production Readiness
**Goal:** Stable, observable, distributable.

1. Health endpoint — sync status, uptime, counters
2. Systemd integration — sd_notify, watchdog
3. Example systemd unit file
4. Example config file with documentation
5. Makefile — build, test, lint targets
6. CI — GitHub Actions (build + test on amd64/arm64, lint)
7. goreleaser — release binaries for linux/amd64 and linux/arm64

**Milestone:** Tagged v0.1.0 release with binaries.

## CLI Interface

```
nextcloud-sync-daemon [flags]

Flags:
  --config PATH    Path to config file (default: see config resolution order)
  --once           Run a single sync and exit (useful for testing)
  --validate       Validate config and exit
  --version        Print version and exit
```

Minimal surface area. All behaviour is driven by the config file.

## Security Considerations

- **Password file permissions**: Warn (not error) if password file is group/world-readable
- **Webhook secret**: Required if webhook is enabled. Compared with constant-time comparison
- **Health endpoint**: Binds to localhost by default. No authentication (status information only)
- **No shell execution**: nextcloudcmd is called via `exec.Command`, not through a shell. Config values are passed as arguments, not interpolated into a command string
- **Public repository**: No deployment-specific values (IPs, hostnames, credentials) in code or tests. Example configs use `example.com` domains
