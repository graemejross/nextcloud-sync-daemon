# nextcloud-sync-daemon

Event-driven sync daemon for headless Nextcloud servers. Wraps `nextcloudcmd` with filesystem watching, webhook push notifications, and configurable polling fallback.

## The Problem

Nextcloud provides excellent sync for desktop and mobile clients, but headless servers are underserved. The available options:

- **Desktop client** — GUI-based, polls every ~30 seconds. Requires a display server. Not suitable for headless servers.
- **`nextcloudcmd`** — CLI tool that performs a single sync and exits. No daemon mode, no file watching, no push notifications. To achieve continuous sync, you must call it repeatedly from cron or a loop.

For headless Linux servers (VMs, Raspberry Pis, NAS devices), this means sync latency is entirely determined by your cron interval. A 5-minute cron gives 5-minute latency. A 30-second cron gives 30-second latency but generates unnecessary load when nothing has changed.

There is no way to achieve low-latency, event-driven sync on a headless server using the standard Nextcloud tooling.

## Background

This project grew out of a production Nextcloud deployment syncing files across two headless Debian servers — a primary VM (Proxmox) and a contingency Raspberry Pi 5. Both run `nextcloudcmd` to keep a local directory in sync with a specific user's folder on the Nextcloud server.

The initial setup was simple: a cron job calling `nextcloudcmd` every minute, plus a fragile `while true; do sync; sleep 5; done &` background loop on one host. This had several problems:

- **High latency** — changes took up to 60 seconds to propagate
- **Fragile** — the background loop died on reboot with no recovery
- **Wasteful** — sync ran every few seconds even when nothing changed
- **One-directional awareness** — local changes were detected only by the polling interval; server-side changes (file uploads via phone or web) had no notification path to the client

We needed event-driven sync in both directions, with a polling fallback for reliability.

## What We Built (Bash/Python Prototype)

The solution is a three-layer sync architecture, deployed identically on both servers and managed as systemd user services.

### Architecture

```
                    ┌──────────────────────────────────────────┐
                    │           Nextcloud Server               │
                    │                                          │
                    │  ┌──────────────────────────────────┐    │
                    │  │    webhook_listeners app          │    │
                    │  │                                   │    │
                    │  │  Events:                          │    │
                    │  │   - NodeCreatedEvent              │    │
                    │  │   - NodeWrittenEvent              │    │
                    │  │   - NodeDeletedEvent              │    │
                    │  │   - NodeRenamedEvent              │    │
                    │  └──────────┬───────────────────────┘    │
                    │             │ POST with shared secret    │
                    └─────────────┼────────────────────────────┘
                                  │
          ┌───────────────────────┼───────────────────────────┐
          │  Headless Server      │                            │
          │                       ▼                            │
          │  ┌─────────────────────────────────────────────┐   │
          │  │  Layer 2: Webhook Listener (port 8767)      │   │
          │  │  Receives server push events                 │   │
          │  │  Filters by path, triggers sync              │   │
          │  └──────────────────┬──────────────────────────┘   │
          │                     │                              │
          │                     ▼                              │
          │  ┌─────────────────────────────────────────────┐   │
          │  │            nextcloudcmd --silent             │   │
          │  │         (bidirectional file sync)            │   │
          │  └─────────────────────────────────────────────┘   │
          │                     ▲            ▲                 │
          │                     │            │                 │
          │  ┌──────────────────┴──┐  ┌──────┴──────────────┐  │
          │  │ Layer 1: inotifywait│  │ Layer 3: systemd    │  │
          │  │ Watches local files │  │ timer (polling      │  │
          │  │ Debounce + cooldown │  │ fallback)           │  │
          │  └─────────────────────┘  └─────────────────────┘  │
          │                                                    │
          │  ~/nextcloud/  ◄── local sync directory            │
          └────────────────────────────────────────────────────┘
```

### Layer 1: Filesystem Watcher (Bash + inotifywait)

Monitors the local sync directory for file changes using Linux's inotify subsystem. When files are created, modified, deleted, or moved locally, it triggers an immediate `nextcloudcmd` sync to push changes to the server.

Key behaviours:

- **Recursive monitoring** — watches all subdirectories
- **Event filtering** — responds to `close_write`, `create`, `delete`, and `move` events
- **Exclusions** — ignores Nextcloud's internal sync database files (`.sync_*.db`)
- **Debounce (10s)** — batches rapid filesystem events into a single sync trigger, preventing unnecessary repeated syncs during bulk file operations
- **Cooldown (30s)** — enforces a minimum interval between syncs using a file-based timestamp, preventing sync storm loops (see [Lessons Learned](#lessons-learned))

Runs as a systemd user service (`nextcloud-watch.service`).

### Layer 2: Webhook Listener (Python)

An HTTP server that receives POST requests from Nextcloud's `webhook_listeners` app when files change on the server. This covers the reverse direction — when files are uploaded via the Nextcloud web UI, mobile app, or another client, the server pushes a notification to trigger an immediate local sync.

Key behaviours:

- **Shared secret authentication** — validates `X-Webhook-Secret` header on every request
- **Path filtering** — only triggers sync for changes within the relevant user's directory
- **Health check endpoint** — responds to GET requests for monitoring
- **Non-blocking sync** — launches `nextcloudcmd` as a subprocess to avoid blocking the HTTP server

The Nextcloud server is configured with webhook listeners for four event types:
- `OCP\Files\Events\Node\NodeCreatedEvent`
- `OCP\Files\Events\Node\NodeWrittenEvent`
- `OCP\Files\Events\Node\NodeDeletedEvent`
- `OCP\Files\Events\Node\NodeRenamedEvent`

Runs as a systemd user service (`nextcloud-webhook.service`).

### Layer 3: Polling Fallback (systemd timer)

A systemd timer that triggers `nextcloudcmd` at a fixed interval as a safety net. If either event-driven layer misses a change (inotifywait restarts, webhook delivery fails, network interruption), the polling fallback ensures sync eventually converges.

The interval is configurable — our deployment uses 15 seconds on one host and 60 seconds (via cron) on the other.

Runs as a systemd timer (`nextcloud-sync.timer`) triggering a oneshot service (`nextcloud-sync.service`).

### Sync Script

All three layers call the same sync script, which wraps `nextcloudcmd` with:

- **PID-based lock file** — prevents overlapping sync runs when multiple layers trigger simultaneously
- **Stale lock detection** — automatically cleans up lock files from dead processes
- **Silent mode** — suppresses `nextcloudcmd`'s verbose debug output (see [Lessons Learned](#lessons-learned))
- **Timestamped logging** — records sync start/complete events

### Systemd Integration

All components run as systemd user services (no root required). User lingering is enabled so services start at boot without requiring a login session.

| Unit | Type | Purpose |
|------|------|---------|
| `nextcloud-watch.service` | `simple` | Filesystem watcher (Layer 1) |
| `nextcloud-webhook.service` | `simple` | Webhook listener (Layer 2) |
| `nextcloud-sync.service` | `oneshot` | Sync execution (called by timer) |
| `nextcloud-sync.timer` | `timer` | Polling fallback (Layer 3) |

All long-running services use `Restart=on-failure` with a 5-second restart delay.

### Log Rotation

A logrotate configuration covers all three log files (`sync.log`, `watch.log`, `webhook.log`) with daily rotation, 7-day retention, 100MB maximum size, and `copytruncate` to avoid interrupting running services.

## Lessons Learned

Building and operating this prototype revealed several issues that inform the design of the Go daemon.

### Sync Storm Loop

The most significant issue. When an external process updates a file on the Nextcloud server frequently (in our case, a capture log updating every ~3 seconds), the following loop occurs:

1. Nextcloud server receives the updated file
2. Webhook listener receives the event, triggers `nextcloudcmd`
3. `nextcloudcmd` pulls the updated file to the local directory
4. `inotifywait` detects the local file change, triggers `nextcloudcmd` again
5. `nextcloudcmd` runs but finds nothing new to sync
6. Meanwhile, the file has been updated again on the server
7. Go to step 2

This produced ~20 syncs per minute with no useful work being done. The fix was the 30-second cooldown in the filesystem watcher — after triggering a sync, it ignores further events for 30 seconds. This breaks the loop while still providing reasonable latency for genuine local changes.

The Go daemon should handle this natively with a unified event queue and deduplication, rather than relying on the filesystem watcher to independently rate-limit itself.

### Log Volume

`nextcloudcmd` produces verbose debug output by default — approximately 3.5 GB per day in our deployment. Without log rotation, this filled a 100 GB disk in under a month. The `--silent` flag suppresses this output, but at the cost of losing visibility into which files were actually transferred.

The Go daemon should implement structured logging with configurable verbosity levels, separating sync metadata (what was transferred, when, how long) from protocol debug output.

### Lock File Races

The PID-based lock file prevents overlapping syncs but has a race window: if the sync script is killed between writing the lock file and registering the trap, a stale lock file is left behind. The current implementation handles this by checking whether the recorded PID is still alive, but this adds complexity.

The Go daemon should use a single process managing all sync triggers through an internal queue, eliminating the need for inter-process coordination entirely.

### Credential Management

The prototype sources credentials from a shell file (`~/.credentials`) containing environment variables. This works but means credentials are visible in the process environment. The Go daemon should support reading credentials from a dedicated config file with appropriate permissions.

## Goal: Go Daemon

This project aims to rewrite the prototype as a single Go binary that replaces the four-component architecture (three scripts plus logrotate) with one daemon.

### Design Principles

1. **Single binary, zero dependencies** — no runtime requirements beyond `nextcloudcmd` being installed. No Python, no inotify-tools package, no separate logrotate configuration.
2. **Drop-in replacement** — works with existing Nextcloud servers and the standard `nextcloudcmd` tool. Does not reimplement the sync protocol.
3. **Unified event loop** — all sync triggers (filesystem events, webhooks, polling timer) feed into a single queue with deduplication and rate limiting, eliminating the sync storm problem by design.
4. **Cross-platform** — compiles for `linux/amd64`, `linux/arm64` (Raspberry Pi), and other platforms supported by Go's `fsnotify` library.
5. **Systemd-native** — runs as a single service unit. Supports `Type=notify` for readiness signalling. Logs to stdout/stderr for journal integration.
6. **Configuration file** — single YAML or TOML file for all settings (server URL, credentials, watch path, intervals, webhook port, log verbosity).

### Planned Features

| Feature | Prototype | Go Daemon |
|---------|-----------|-----------|
| Filesystem watching | `inotifywait` (external) | `fsnotify` (built-in) |
| Webhook listener | Python HTTP server (separate process) | Built-in HTTP server (same process) |
| Polling fallback | systemd timer (external) | Built-in ticker (same process) |
| Sync execution | Calls `nextcloudcmd` via shell script | Calls `nextcloudcmd` directly |
| Event deduplication | File-based cooldown timestamp | In-memory event queue with configurable window |
| Debounce | `read -t` timeout in bash | Timer-based coalescing |
| Lock management | PID file with stale detection | Not needed (single process) |
| Log rotation | External logrotate config | Built-in size/count rotation, or log to journal |
| Credential storage | Sourced shell environment file | Config file with restricted permissions |
| Health check | HTTP GET on webhook port | Dedicated health endpoint with sync status |
| Metrics | None | Optional Prometheus metrics endpoint |

### Example Configuration (Draft)

```yaml
server:
  url: https://nextcloud.example.com
  username: sync-user
  password_file: /etc/nextcloud-sync/password  # or inline password

sync:
  local_dir: /home/user/nextcloud
  remote_path: /
  nextcloudcmd: /usr/bin/nextcloudcmd
  extra_args: ["--silent"]

watch:
  enabled: true
  debounce: 10s
  cooldown: 30s
  exclude:
    - '\.sync_.*\.db'

webhook:
  enabled: true
  listen: 0.0.0.0:8767
  secret: your-shared-secret
  path_filter: "/username/"

poll:
  enabled: true
  interval: 60s

logging:
  level: info  # debug, info, warn, error
  format: text  # text, json
```

### Non-Goals

- **Reimplementing the Nextcloud sync protocol.** The daemon wraps `nextcloudcmd`, not replaces it. The sync protocol is complex (chunked uploads, conflict resolution, metadata propagation) and already well-implemented.
- **Replacing the desktop client.** This tool is for headless servers. If you have a display server, use the official Nextcloud desktop client.
- **Multi-account sync.** The daemon syncs one local directory with one remote path. Run multiple instances for multiple accounts.

## Development Approach

This project is being built using [Claude Code](https://claude.ai/claude-code) as a practical test of AI-assisted development. The original prototype (bash scripts and Python webhook listener) was also developed with Claude Code in a production environment, including the debugging of the sync storm issue and the iterative improvements to debounce and cooldown logic.

The rewrite to Go is an intentional challenge: take a working but fragile multi-component prototype and produce a clean, distributable single-binary tool — with AI assistance throughout the design, implementation, and testing process. The development history, including the decisions, mistakes, and iterations, is documented transparently.

## Status

**Pre-development.** The bash/Python prototype is running in production. The Go rewrite has not yet started.

## Requirements

### Prototype (current)
- Linux (inotifywait uses the Linux inotify subsystem)
- `nextcloudcmd` (from the `nextcloud-desktop-cmd` package)
- `inotify-tools` (provides `inotifywait`)
- Python 3 (for the webhook listener)
- systemd (for service management)

### Go Daemon (planned)
- `nextcloudcmd`
- That's it.

## License

MIT
