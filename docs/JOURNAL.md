# Development Journal

This project is built using [Claude Code](https://claude.ai/claude-code) as a practical demonstration of AI-assisted software development. This journal records the full development narrative — decisions, problems, solutions, and lessons — as they happen.

Each entry corresponds to a session of work. GitHub issues contain the technical implementation details; this journal captures the higher-level story.

---

## Session 1 — 2026-03-16: Project Setup & Phase 1

**Issues:** #4 (scaffolding, closed), #5 (Phase 1, closed)
**Commits:** `11955f9`, `587f8d1`, `cdc3f50`
**Duration:** Single session

### What happened

Started from a repo containing only a README, design document (DESIGN.md), and prototype scripts. The goal was to create GitHub issues for all 5 phases and implement Phase 1 — a working `--once` mode that loads config and runs nextcloudcmd once.

Created 6 issues (#4–#9) covering the full roadmap from scaffolding through production readiness. Then implemented Phase 1 in two commits: scaffolding first, then config + sync executor + CLI.

### Key decisions

**All config sections defined in Phase 1.** Even though only Server, Sync, and Logging are used by `--once` mode, the full config schema (Watch, Webhook, Poll, Health) is defined and validated from day one. Users can write their complete config file immediately and validate it with `--validate`. This avoids config format changes between phases.

**Boolean defaults left as false.** Go's zero value for bool is `false`, and YAML `false` decodes to Go `false` — there's no way to distinguish "not set" from "explicitly disabled" without pointer types or a sentinel. Rather than adding complexity, event sources default to disabled and users must explicitly enable them. This is documented in the example config.

**TestHelperProcess pattern for executor tests.** The standard Go pattern where the test binary invokes itself as a subprocess. More robust than creating a fake shell script with hardcoded behaviour, and avoids needing a real nextcloudcmd installation for tests.

**Non-zero exit code is not a Go error.** `executor.Run()` returns `(result, nil)` when nextcloudcmd exits non-zero — the exit code is in `result.ExitCode` and `result.Error`. A Go-level error is only returned for things that prevent execution (missing password file, timeout, exec failure). This lets callers distinguish "sync ran but found problems" from "sync couldn't run at all".

### Problems encountered

1. **Go not installed.** Development machine didn't have Go. Installed 1.22.12 to `/usr/local/go/`.

2. **`.gitignore` swallowed the source code.** The pattern `nextcloud-sync-daemon` (no leading slash) matched the `cmd/nextcloud-sync-daemon/` directory, hiding all CLI source files from git. Fixed to `/nextcloud-sync-daemon` (leading slash = root-level binary only). Caught because `git status` didn't show the cmd/ files.

3. **YAML Duration unmarshal order matters.** Integer `120` in YAML decodes successfully as string `"120"` — which then fails `time.ParseDuration("120")` because Go requires a unit suffix. Fix: check `value.Tag == "!!int"` and try integer decode first.

4. **TestHelperProcess subprocess args.** First attempt used `os.Args[0]` directly as the nextcloudcmd path, but the test binary receives `-test.run=TestHelperProcess` args that confused the helper. Fix: create a shell wrapper script in `t.TempDir()` that calls `os.Args[0] -test.run=TestHelperProcess -- "$@"`, so the `--` separates test framework args from nextcloudcmd args.

### Lessons

- Always check `.gitignore` patterns against directory names, not just file names. A pattern without a leading slash matches anywhere in the path.
- The TestHelperProcess pattern needs a shell wrapper when the test binary is used as a fake command — you can't pass it directly because the test runner's own flags interfere.
- YAML type coercion is aggressive. Always check the YAML node tag when you need to distinguish integers from strings.

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 91.8% | 25 |
| sync | 87.7% | 8 |
| cmd | 0% | 0 (thin wiring, no independent logic) |
| daemon | n/a | no test files (types only) |

Decision: no additional tests needed for Phase 1. cmd/ coverage will come naturally with integration tests in Phase 5.

### Process observation

The initial implementation didn't document anything on the GitHub issues — just a one-line close comment on #4. This was caught and corrected: detailed implementation comments were added to both #4 and #5 after the fact, and an "Issue Documentation Rule" was added to the topic's CLAUDE.md. Going forward, issues are updated as work progresses, not retroactively.

---

## Session 1 (continued) — 2026-03-16: Phase 2

**Issue:** #6 (event loop engine with polling)
**Commits:** `2476563`

### What happened

Implemented the event loop engine and poller — the two components needed for continuous daemon mode. After Phase 1, the binary could only run `--once`. Now it runs as a long-lived daemon, syncing on a configurable poll interval and shutting down cleanly on SIGTERM/SIGINT.

### Implementation

**Poller** was trivial — 50 lines. `time.NewTicker` in a select loop with non-blocking send to the trigger channel. The non-blocking send (`select` with `default`) is essential: without it, the poller would block when the engine is mid-sync.

**Engine** was the interesting piece. The DESIGN.md spec translated cleanly into Go:
- Capacity-1 event channel handles coalescing automatically — when the engine is busy syncing, multiple events collapse into at most one pending event
- Cooldown is a simple `time.Since(lastSync) < cooldown` check, with a `lastSync.IsZero()` exception for the first event
- Sources start in goroutines tracked by `sync.WaitGroup` for clean shutdown
- Counters are local to the main loop (single goroutine) — no atomics needed there

**CLI** changes were minimal — remove the "not yet implemented" guard, build sources from config, create engine, run it.

### Key decisions

**Cooldown applies after failures too.** If nextcloudcmd is broken, rapid retries won't help. Better to wait for the next natural trigger. This matches the prototype's behaviour.

**Source errors don't stop the engine.** If one event source crashes (e.g., watcher), the others continue. The engine logs the error but keeps running. This is important for resilience — a crashed watcher shouldn't kill a working poller.

**No problems encountered.** This phase went smoothly — the interfaces from Phase 1 fit exactly as designed. All tests passed on the first run. The TestHelperProcess pattern from Phase 1 wasn't needed here because the engine tests use fake executors (simple structs), not real subprocesses.

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 91.8% | 25 |
| engine | 97.0% | 7 |
| poller | 100.0% | 4 |
| sync | 87.7% | 8 |

### Process improvement

This time, issue #6 was documented properly — a "starting work" comment at the beginning with the plan, and a detailed implementation comment at the end with everything that was built, decided, and learned. Following the Issue Documentation Rule established after Phase 1.

---

## Session 1 (continued) — 2026-03-16: Phase 3

**Issue:** #7 (filesystem watcher)
**Commits:** `a11d674`

### What happened

Implemented the filesystem watcher — the second EventSource, and the most complex one. This adds local file change detection: editing a file in the sync directory triggers an immediate sync (after debounce).

### Implementation

The watcher wraps `fsnotify` with three layers of logic:

1. **Recursive directory watches.** fsnotify only watches individual directories, not trees. `addRecursive()` walks the directory tree at startup and adds a watch on every directory. When a `Create` event fires for a new directory, `addRecursive()` is called again to watch it. This handles the common case of creating a nested directory structure (e.g., `mkdir -p a/b/c`).

2. **Compiled exclude patterns.** Exclude patterns from config are compiled into `[]*regexp.Regexp` at construction time. Every filesystem event is checked against these patterns using the relative path. This catches Nextcloud's internal `.sync_*.db` files that would otherwise trigger constant syncs.

3. **Timer-reset debounce.** The most interesting piece. Uses a nil-channel trick:
   - `debounceC` starts as nil — `select` on a nil channel blocks forever, so the debounce case is invisible
   - First filesystem event creates a timer and points `debounceC` at its channel
   - Subsequent events stop+drain+reset the timer
   - When the timer fires, the event is sent and both variables reset to nil

   This batches bulk operations (git checkout, drag-and-drop 50 files) into a single sync trigger.

### Key decisions

**Exclude patterns match relative paths, not absolute.** The pattern `\.sync_.*\.db` matches both `foo/.sync_abc.db` and `.sync_abc.db`. This is more intuitive for users writing config — they don't need to know the full absolute path.

**Directory watch failures are warnings, not errors.** If one directory in a tree can't be watched (permissions, inotify limits), the rest of the tree still works. Partial coverage is better than crashing.

**Timer drain is necessary.** When calling `timer.Stop()`, if it returns false, the timer has already fired and the value is sitting in the channel. Without draining it, the next `select` iteration would immediately see it and fire a premature debounce. The `select` with `default` handles the race where another goroutine already consumed it.

### Problems encountered

1. **Missing `fmt` import** — test file used `fmt.Sprintf` for filename generation but the import was missing. Had a `var _ = fmt.Sprintf` at the bottom that was supposed to force the import but didn't work without the actual import statement. Minor.

2. **go.mod cache not synced** — `go get` ran but `go mod tidy` was needed to update go.sum. The `go get` had succeeded earlier but the module graph wasn't resolved until tidy ran.

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 91.8% | 25 |
| engine | 97.0% | 7 |
| poller | 100.0% | 4 |
| sync | 87.7% | 8 |
| watcher | 78.3% | 9 |

The watcher's uncovered 21.7% is error-handling code (fsnotify internal errors, filepath.Rel failures, directory walk errors) that can't be triggered with real filesystems in unit tests. All user-facing logic paths are covered.

### Design observation

The `daemon.EventSource` interface continues to prove its worth. The watcher is significantly more complex than the poller (150 lines vs 50), but it plugs into the engine identically. The engine doesn't know or care whether events come from fsnotify or a timer — it just reads from the channel and applies cooldown. This is exactly the decoupling the interfaces were designed for.

---

## Session 1 (continued) — 2026-03-16: Phase 4

**Issue:** #8 (webhook listener)
**Commits:** `73565b4`

### What happened

Implemented the third and final EventSource — an HTTP server that receives Nextcloud `webhook_listeners` push notifications. This completes the three-layer sync architecture from the prototype: filesystem watcher (local changes), webhook (server changes), and polling (fallback).

### Implementation

The webhook server is conceptually simpler than the watcher — it's a standard HTTP handler with a few Nextcloud-specific concerns:

1. **Secret validation** with `crypto/subtle.ConstantTimeCompare` — prevents timing attacks that could leak the shared secret byte-by-byte.
2. **Lenient payload parsing** — if the JSON is malformed, we still trigger a sync. A valid POST with the correct secret means something changed on the server. The prototype took the same approach.
3. **Path filtering** using `strings.Contains` — matches the prototype's `if PATH_FILTER in path`. Simple but correct for the use case.
4. **Body size limit** — `io.LimitReader` at 1MB prevents memory exhaustion from oversized payloads.

The server lifecycle uses `net.Listen` + `srv.Serve` (not `ListenAndServe`) so we can bind the port before starting the shutdown goroutine. The shutdown goroutine watches `ctx.Done()` and calls `srv.Shutdown()` with a 5-second grace period.

### Key decisions

**Malformed JSON still triggers sync.** If someone sends a valid POST with the correct secret but garbage JSON, we sync. The alternative (reject and don't sync) risks missing legitimate changes if Nextcloud ever changes its payload format. Defensive: sync too much rather than too little.

**Path filter returns 200 "filtered", not 4xx.** A filtered event is not an error — the server received and acknowledged the webhook correctly, it just decided the change isn't relevant. Returning an error code would cause Nextcloud to retry the webhook.

**1MB body limit, not configurable.** Nextcloud webhook payloads are tiny (a few hundred bytes). 1MB is generous enough to never reject a legitimate payload, small enough to prevent abuse. No reason to make it configurable.

### Testing approach

Used `httptest.NewRequest` + `httptest.NewRecorder` for handler logic (13 unit tests), plus two integration tests with a real HTTP server on port 18767. The integration tests verify the full server lifecycle (start, serve, shutdown) and real HTTP round-trips.

Table-driven `extractPath` tests cover 6 payload variants (valid, empty path, missing node, empty object, invalid JSON, empty body).

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 91.8% | 25 |
| engine | 97.0% | 7 |
| poller | 100.0% | 4 |
| sync | 87.7% | 8 |
| watcher | 78.3% | 9 |
| webhook | 89.5% | 15 |

**Total: 68 tests across 6 packages.** All three EventSource implementations complete.

### Architecture complete

With the webhook, all three event sources from the prototype are now implemented in Go:

| Prototype | Go Daemon | Status |
|-----------|-----------|--------|
| `inotifywait` bash script + systemd service | `internal/watcher/` | ✓ |
| Python HTTP server + systemd service | `internal/webhook/` | ✓ |
| systemd timer + oneshot service | `internal/poller/` | ✓ |
| bash sync script with lock file | `internal/sync/` + `internal/engine/` | ✓ |

The daemon is now functionally equivalent to the prototype's four-component architecture, in a single binary. Phase 5 adds production polish (health endpoint, systemd sd_notify, CI, releases).

---

## Session 1 wrap-up — 2026-03-16: Phase 5 Planning

**Issue:** #9 (production readiness)

### What remains

Phase 5 is the final phase — four components, mostly config files rather than application code:

1. **Health endpoint** (`internal/health/`) — the only component with significant code. `Status` struct with `sync.RWMutex`, tracks uptime, last sync result, sync/fail counts, source status. JSON response on GET. Engine needs updating to call `RecordSync()` and `SetSourceRunning()`.

2. **Systemd integration** — `go-systemd` for `sd_notify(READY=1)` after sources start, watchdog heartbeat every 60s, `STOPPING` on shutdown. Example unit file with hardening directives.

3. **GitHub Actions CI** — build + test matrix (amd64/arm64), golangci-lint. Standard Go CI workflow.

4. **goreleaser** — `.goreleaser.yml` for linux/amd64 + linux/arm64 binaries. Release workflow triggered by tag push. Tag v0.1.0 when ready.

### Session statistics

- **Phases completed:** 1, 2, 3, 4 (out of 5)
- **Packages:** 6 (daemon, config, sync, engine, poller, watcher, webhook)
- **Tests:** 68
- **Lines of Go:** ~2,900
- **Commits:** 14 (10 in daemon repo, 4 in docs repo)
- **Issues closed:** 7 (#1-8 except #3 and #9)
- **Token usage:** ~718k tokens, ~$34.60

---

## Session 2 — 2026-03-16: Phase 5

**Issue:** #9 (production readiness)
**Commits:** `da22949`, `0187e8f`, `233248f`

### What happened

Implemented all Phase 5 components: health endpoint, systemd integration, CI pipeline, and release automation. This completes the project — the daemon is ready for v0.1.0.

### Implementation

**Health endpoint** (`internal/health/`) — the only new package with significant code. `Status` struct with `sync.RWMutex` tracking uptime, last sync result, sync/fail counts, and source running state. JSON response via `http.HandlerFunc`. 100% test coverage including a concurrent access hammer test (150 goroutines).

**Engine wiring** — `engine.New()` now takes an optional `*health.Status` parameter. After each sync, calls `health.RecordSync()`. Source goroutines call `SetSourceRunning(true)` on start, `false` on stop via defer. Added `OnReady` callback field — called after sources start but before the main loop blocks. Existing tests pass `nil` for health (backward compatible).

**Systemd integration** — Added `github.com/coreos/go-systemd/v22/daemon` (aliased as `sdnotify` to avoid conflict with `internal/daemon`). In `main.go`: `SdNotifyReady` via the `OnReady` callback, watchdog goroutine at half `WatchdogSec`, `SdNotifyStopping` on shutdown. Health HTTP server starts before the engine and shuts down on context cancellation.

**CI** — GitHub Actions workflow: test + golangci-lint on push to main and PRs. Uses `golangci/golangci-lint-action@v6` for reliable linter installation.

**goreleaser** — `.goreleaser.yml` v2 config for `linux/amd64` + `linux/arm64`. CGO disabled. Release workflow triggered on tag push (`v*`).

### Key decisions

**Go 1.23 upgrade.** `go-systemd v22.7.0` requires Go 1.23+. Go's toolchain directive handles this automatically — Go 1.22 (installed) downloads 1.23 as needed. Updated go.mod accordingly.

**Health response uses pointer fields for optional data.** `last_sync`, `last_sync_duration_ms`, and `last_sync_trigger` are `*string`/`*int64` so they serialize as `null` (omitted) when no syncs have run yet, rather than empty/zero values.

**Status logic: "ok" unless last sync failed.** Simple rule: `failCount` alone doesn't trigger "degraded" — only a failed *most recent* sync does. This means recovering from a failure (next sync succeeds) immediately returns to "ok".

### Problems encountered

1. **`go-systemd` module not found after `go get`.** Needed `go mod tidy` after `go get` to resolve the module graph — same issue as Phase 3 with fsnotify.

2. **Import alias required.** `github.com/coreos/go-systemd/v22/daemon` would conflict with `internal/daemon`. The Phase 5 prompt anticipated this — used `sdnotify` alias as planned.

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 91.8% | 25 |
| engine | 95.3% | 9 |
| health | 100.0% | 10 |
| poller | 100.0% | 4 |
| sync | 87.7% | 9 |
| watcher | 78.3% | 9 |
| webhook | 89.5% | 14 |

**Total: 80 tests across 7 packages.** Up from 68 tests in Phase 4.

### Project complete

All five phases are implemented. The daemon is a complete, single-binary replacement for the prototype's four-component architecture:

| Component | Status |
|-----------|--------|
| Config loading + validation | Phase 1 ✓ |
| Sync executor (nextcloudcmd wrapper) | Phase 1 ✓ |
| Event loop engine with coalescing | Phase 2 ✓ |
| Polling fallback | Phase 2 ✓ |
| Filesystem watcher | Phase 3 ✓ |
| Webhook listener | Phase 4 ✓ |
| Health endpoint | Phase 5 ✓ |
| Systemd integration | Phase 5 ✓ |
| CI pipeline | Phase 5 ✓ |
| Release automation | Phase 5 ✓ |

---

## Session 2 (continued) — 2026-03-16: Security Hardening

**Issues:** #10 (webhook rate limiting), #11 (password file perms), #12 (password in ps), #13 (health endpoint warning)

### What happened

Conducted a security audit of the v0.1.0 release before considering community contribution to the Nextcloud ecosystem. The audit identified three medium-risk issues and one low-risk issue. All four were fixed in this session.

### Findings and fixes

**Webhook replay attacks (#10).** The webhook endpoint had no protection against an attacker replaying valid requests to trigger excessive syncs. Added per-IP rate limiting — a `map[string]time.Time` with a mutex, enforcing a 5-second minimum interval between requests from the same source IP. Returns 429 Too Many Requests for rate-limited requests. The rate limiter sits after secret validation (so unauthenticated requests are rejected before hitting the rate limiter) but before body parsing (so rate-limited requests don't waste time parsing payloads).

**Password file permissions (#11).** Added `CheckPasswordFilePermissions()` to the config package. At startup, if `password_file` is configured, the daemon stats the file and warns if group or other read bits are set (mode & 0077 != 0). Warning only, not a hard failure — some deployments may intentionally use group-readable files.

**Password in process list (#12).** This is a `nextcloudcmd` limitation — it only accepts passwords via `-p` command-line argument, visible in `ps`. Cannot be fixed without upstream changes. Documented the risk in README (new Security Considerations section), example config, and example systemd unit. Added `ProtectProc=invisible` to the systemd service file (systemd 247+), which hides `/proc` entries from other users.

**Health endpoint information disclosure (#13).** Added a startup warning if the health endpoint is bound to a non-localhost address. Updated example config with a WARNING comment about what the endpoint exposes.

Also fixed a hostname leak in JOURNAL.md — "clarence" (a real infrastructure hostname) was mentioned in the Phase 1 problems section. Replaced with "Development machine".

### Key decisions

**Rate limit interval is hardcoded at 5 seconds.** Nextcloud sends webhooks per-event, not in bursts. 5 seconds is generous enough for legitimate use (Nextcloud wouldn't fire the same event type from the same source more than once per 5 seconds) while preventing replay floods. Not worth making configurable — it's a security control, not a user preference.

**Password file check is a warning, not an error.** Hard-failing on permissions would break deployments using group-readable password files (e.g., shared between services). The warning gives operators the information; they decide the appropriate action.

### Test coverage

Added `TestWebhookRateLimiting` — tests same-IP blocking, different-IP passes, and recovery after cooldown expires. Added `TestCheckPasswordFilePermissions` — tests 0600 (safe), 0644 (warns), no password file, and missing file.

| Package | New Tests | Total Tests |
|---------|-----------|-------------|
| webhook | +1 | 16 |
| config | +4 | 29 |
| **Total** | **+5** | **85** |

---

## Session 3 — 2026-03-16: Team Review & Migration

**Issues:** #14 (README restructure, closed), #15 (team review fixes, closed), #16 (clarence/silver-pi migration, closed)
**Commits:** `562c5ed`, `64ccc66`, `f411052`

### What happened

Three phases of work in this session: restructuring the README for an end-user audience, a multi-agent critical review of the entire repo before community announcement, and migrating clarence and silver-pi from the prototype to the Go daemon.

### README restructure (#14)

The README was rewritten from a development-focused design document to a user-facing tool README. New structure: Features, Requirements, Installation (download + build from source), Configuration (minimal config, password file, validate/test), Usage (CLI flags table), Running as systemd service (system-wide + user service), Webhook setup (4-step guide with curl commands), Security Considerations.

The development history (the problem, the prototype architecture diagram, prototype vs Go daemon comparison table, lessons learned) was preserved below a horizontal rule. This respects the project's commitment to transparent AI-assisted development documentation while putting user-relevant content first.

### Team critical review (#15)

Dispatched a 4-agent team (Oliver/Opus, Sophie/Sonnet, Cedric/Codex, Grace/Gemini) for a pre-announcement critical review. Three agents returned actionable findings; Grace (Gemini) couldn't access the repo.

**7 issues found and fixed:**

1. **Missing LICENSE file** (Critical) — README claimed MIT but no LICENSE file existed. GitHub showed "No license". Added MIT LICENSE file.

2. **Unbounded rate-limit map** (Medium) — `webhook.go` rateMap grew forever with no eviction. Added periodic cleanup every 10 minutes, purging entries older than the rate interval.

3. **Leaked cancel function** (Medium) — `context.go` discarded the cancel function from `signal.NotifyContext`. Fixed to return it so callers can defer it.

4. **`--once` fails without event sources** (Medium) — `config.Validate()` required at least one event source, but `--once` mode doesn't need sources. Split into `Validate()` (core checks) and `ValidateEventSources()` (called only in daemon mode).

5. **Health server shutdown no timeout** (Low) — Used `context.Background()` which could block forever. Added 5-second timeout matching the webhook server pattern.

6. **systemd `WantedBy=default.target`** (Low) — For headless servers, `multi-user.target` is conventional. Changed.

7. **CI actions on mutable tags** (Low) — Pinned `actions/checkout`, `actions/setup-go`, `golangci-lint-action`, and `goreleaser-action` to SHA.

**Findings dismissed after verification:**
- "Password visible in ps" — already documented in Security Considerations
- "Double-counting failures" — engine and health have independent counters by design
- "OnReady fires before sources ready" — acceptable; watchdog catches stalls
- "Webhook default 0.0.0.0" — intentional; webhooks need external access
- ".gitignore too narrow" — adequate for a Go project
- "POST-COMPACT-CONTEXT in repo" — already gitignored

### Migration to clarence and silver-pi (#16)

Migrated both hosts from the bash/Python prototype to the Go daemon. These are the most active sync clients — the best real-world stress test before community announcement.

**Clarence (amd64):**
- Stopped and disabled `nextcloud-watch` and `nextcloud-webhook` prototype units
- Deployed v0.1.2-pre binary
- Config: watcher + webhook + poller, 5-minute poll interval, health on port 8769 (8768 occupied by Nextcloud MCP server)
- Existing webhook registrations (IDs 25-36) reused — same IP, port, secret

**Silver-pi (arm64):**
- Stopped and disabled `nextcloud-watch`, `nextcloud-webhook`, and `nextcloud-sync.timer` prototype units
- Cross-compiled arm64 binary from clarence
- Same config as clarence, health on port 8768
- Existing webhook registrations (IDs 37-40) reused
- Simplified systemd unit — `ProtectProc=invisible` and `ProtectSystem=strict` cause status=226/NAMESPACE on the Pi's kernel

**End-to-end verification:** File created on clarence → watcher triggered sync to Nextcloud → Nextcloud fired webhook to silver-pi → silver-pi synced. Full chain in ~30 seconds.

### Problems encountered

1. **Health port conflict on clarence.** Port 8768 was already used by the Nextcloud MCP server (python3). Changed to 8769.

2. **Password file owned by root.** The daemon runs as user `graeme` but `sudo tee` created the file owned by root. Fixed with `sudo chown graeme:graeme`.

3. **ProtectProc/ProtectSystem on Raspberry Pi.** Silver-pi's kernel doesn't support these systemd namespace directives. The service failed with exit code 226/NAMESPACE. Fixed by using a minimal unit file without security hardening directives.

### Deployment state

| Host | Arch | Version | Sources | Health | Webhook IDs |
|------|------|---------|---------|--------|-------------|
| clarence | amd64 | 0.1.2-pre | watcher + webhook + poller | :8769 | 25-36 |
| silver-pi | arm64 | 0.1.2-pre | watcher + webhook + poller | :8768 | 37-40 |
| gold-pi | arm64 | 0.1.1 | watcher + webhook + poller | :8768 | 41-44, 49-52 |
| chorus | amd64 | 0.1.1 | watcher + webhook + poller | :8768 | 45-48, 53-56 |

Prototype units left disabled (not removed) on clarence and silver-pi as rollback option.

### Test coverage (post-review fixes)

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 92.6% | 34 |
| engine | 95.3% | 9 |
| health | 100.0% | 10 |
| poller | 100.0% | 4 |
| sync | 87.7% | 9 |
| watcher | 78.3% | 9 |
| webhook | 88.5% | 16 |
| **Total** | | **91** |

### Lessons

- **Multi-agent reviews catch real issues.** The LICENSE file omission would have been embarrassing — it's the first thing open-source contributors check. The `--once` validation bug would have frustrated the first user who tried a minimal cron config.
- **Cross-check agent findings.** Several findings sounded plausible but were wrong or already handled. Without verification, we'd have "fixed" things that didn't need fixing.
- **systemd security directives are kernel-dependent.** `ProtectProc=invisible` requires kernel 5.8+ with `hidepid=invisible` support. The Raspberry Pi's kernel (6.1) should support it, but the user-service context may not. Always test on the actual target.
- **Port conflicts are deployment-specific.** The health endpoint port should be documented as "pick a free port" rather than having a fixed default. For now, the default 8768 works on 3 of 4 hosts.

---

## Session 4 — 2026-03-17: Peer Notification, Health Enhancements & Sync Test

**Issues:** #19 (health enhancements), #17 (peer notification), #18 (sync test command)
**Commit:** `21b8bb1`

### What happened

Implemented three features driven by the cross-host sync latency investigation from the previous session. The investigation found that after a local file change, the second host took ~30 seconds to sync — 10 seconds wasted in the Nextcloud server's job queue before it fired the webhook. Peer-to-peer notification eliminates that delay by having the first host POST directly to the second host's webhook endpoint after a successful watcher-triggered sync.

Implementation order: #19 → #17 → #18. Issue #19 added `RecordWebhookReceived()` to the health system, which #17's peer concept builds on conceptually (peer notifications arrive via the same webhook endpoint). Issue #18 was independent but benefits from both.

### Issue #19 — Health endpoint enhancements

Added two capabilities to the health endpoint:

1. **Per-trigger sync counts** (`trigger_counts` in JSON). A `map[string]int64` tracking how many syncs each source triggered — e.g., `{"watcher": 12, "webhook": 3, "poller": 5}`. Incremented in `RecordSync()` when `result.Trigger` is non-empty.

2. **Last webhook received timestamp** (`last_webhook_received`). `RecordWebhookReceived()` called by the webhook server after successful secret validation (before rate limiting and path filtering). This tells operators "yes, webhooks are arriving" even if they're being filtered or rate-limited.

Also moved `healthStatus := health.NewStatus()` out of the `if cfg.Health.Enabled` block — it's always created now. The HTTP server only starts when enabled, but internal tracking runs regardless. This eliminates nil checks throughout the codebase.

### Issue #17 — Peer notification

New `internal/peer/` package. A `Notifier` struct POSTs `{"source":"peer","time":"<RFC3339>"}` to each configured peer's webhook endpoint with the shared `X-Webhook-Secret` header. 5-second HTTP timeout. Concurrent goroutine per peer with `sync.WaitGroup`. Errors logged at Warn, never returned.

**Echo loop prevention** is the critical design constraint. The engine only notifies peers after successful *watcher-triggered* syncs:

```go
if e.notifier != nil && event.Source == "watcher" {
    go e.notifier.NotifyPeers(ctx)
}
```

Not on webhook source (would echo between peers), not on poller (nothing directional happened), not on failure (nothing to tell peers about).

Config:
```yaml
peers:
  - url: "http://silver-pi:8767/webhook"
    secret: "shared-secret"
```

Validation: each peer needs a parseable URL and non-empty secret. Empty list is valid.

### Issue #18 — Sync test command

New `--test` flag: writes `.nsd-sync-test-<timestamp>` marker file, runs a full sync, reports exit code and duration, removes the marker, runs a second sync to propagate the deletion. Uses the same executor as normal daemon syncs — if the test passes, real syncs will work.

### Deployment

Built v0.2.0-pre binaries (amd64 + arm64), deployed to all 4 hosts. Peer mesh configured:

| Host | Peers |
|------|-------|
| clarence | silver-pi:8767, chorus:8767 |
| silver-pi | clarence:8767, chorus:8767 |
| chorus | clarence:8767, silver-pi:8767 |
| gold-pi | (none — different Nextcloud user/path) |

Using Tailscale MagicDNS hostnames. Reusing existing webhook secrets for peer auth.

### Key decisions

- **Watcher-only notification** — the simplest rule that prevents echo loops. All other approaches (tracking notification origin, deduplication tokens) add complexity without benefit.
- **5-second peer timeout** — peers are on the same Tailscale network. If they don't respond in 5 seconds, they're down. The WaitGroup still returns promptly.
- **No retry** — if a peer is down, it'll catch up on its next poll. Retrying complicates the code and delays the sync loop for no benefit.
- **Always create healthStatus** — eliminates nil checks in webhook and engine wiring. The HTTP server is the expensive part; the Status struct is cheap.

### Problems encountered

1. **"Text file busy"** — `sudo cp` failed when overwriting the running binary. Fixed by stopping the service first.
2. **Pre-existing TestFindConfigPath failure** — real config at `/etc/nextcloud-sync-daemon/config.yaml` on deployment hosts causes the test to find system config instead of test config. Not related to these changes.

### Test coverage

| Package | Coverage | Tests |
|---------|----------|-------|
| config | 88.7% | 39 |
| engine | 95.6% | 13 |
| health | 100.0% | 14 |
| peer | 85.7% | 6 |
| poller | 100.0% | 4 |
| sync | 87.7% | 9 |
| watcher | 78.3% | 9 |
| webhook | 88.8% | 18 |
| **Total** | | **~112** |

Up from 91 tests (Session 3) to ~112, across 8 packages. New `peer` package added.

### Lessons

- **Peer notification is the biggest latency win.** Nextcloud's internal job queue adds ~10 seconds before firing webhooks. Direct peer POST eliminates this entirely — the second host starts syncing within 1-2 seconds of the first host's sync completing.
- **Echo loop prevention needs to be designed in, not bolted on.** The `event.Source == "watcher"` guard is simple and correct because the source field was already part of the Event type from Phase 2. If we hadn't had source attribution, preventing loops would have required tracking notification IDs or timestamps — much more complex.

---

## Session 5 — 2026-03-17: notify_push WebSocket Event Source

**Issues:** #24 (implementation), #26 (server setup/integration test)
**Commits:** `def50fc`

### What happened

Community feature request from @ThaDaVos (#23): the webhook listener requires the daemon host to be reachable from the Nextcloud server — doesn't work behind NAT/firewalls. Nextcloud's [notify_push](https://github.com/nextcloud/notify_push) app reverses the connection: the client connects *outbound* via WebSocket, so no inbound ports needed.

Implemented notify_push as the 4th event source, installed the app on the Nextcloud server, integration tested end-to-end, and deployed to all three hosts.

### Implementation

New `internal/notifypush/` package — `Client` struct implementing `daemon.EventSource`:

- **Auto-discovery** — queries `GET /ocs/v2.php/cloud/capabilities?format=json` with Basic Auth, parses nested JSON to find the WebSocket endpoint URL. Re-discovers on each reconnect if URL not configured.
- **Authentication** — sends username (text message), then password (text message), awaits `"authenticated"` response. Uses `cfg.ResolvePassword()` for credential rotation support.
- **Message handling** — `notify_file` and `notify_file_id *` → emit Event; `notify_activity`/`notify_notification` → ignored.
- **Keep-alive** — 30-second WebSocket ping loop.
- **Reconnect** — exponential backoff (5s → 5m, capped). Resets to initial interval after successful authentication (transient disconnect vs connection failure).
- **Dependency** — `nhooyr.io/websocket` v1.8.17 (pure Go, context-aware, no CGo).

### Server-side setup

Installing notify_push on the Nextcloud Docker stack required:

1. `occ app:install notify_push` — installed v1.3.1
2. New `notify-push` container in docker-compose.yml — uses the binary from the installed app, reads config.php, connected to frontend + backend Docker networks
3. Caddy `handle_path /push/*` route proxying to `notify-push:7867`
4. **Trusted proxies fix** — Docker container hostnames ("caddy", "notify-push") don't work for Nextcloud's trusted_proxies check. Had to use CIDR ranges (`172.18.0.0/16 172.19.0.0/16`). The `TRUSTED_PROXIES` env var in docker-compose.yml overrides any `occ config:system:set` changes via `reverse-proxy.config.php`.

### Integration test results

```
14:31:06  Auto-discovered WebSocket endpoint from capabilities API
14:31:06  Connected and authenticated via WSS
14:31:11  Uploaded test file via WebDAV
14:31:11  notify_file received (~800ms after upload)
14:31:11  Sync triggered (source: notify_push)
14:31:13  Sync complete (1650ms total)
```

### Deployment

Built new binaries (amd64 + arm64), deployed to all three hosts, added `notify_push: enabled: true` to each config, restarted services.

| Host | Arch | Sources | notify_push |
|------|------|---------|-------------|
| clarence | amd64 | watcher + webhook + poller + notify_push | connected |
| silver-pi | arm64 | watcher + webhook + poller + notify_push | connected |
| chorus | amd64 | watcher + webhook + poller + notify_push | connected |

All three auto-discovered the WebSocket endpoint and authenticated. notify_push events visible in production logs within minutes.

### Key decisions

- **No peer notification for notify_push events.** The engine only notifies peers for `Source == "watcher"`. This is correct: push events come from the server, so all connected clients get their own notifications independently.
- **`connectAndListen` returns `(authenticated bool, err error)`** — allows the reconnect loop to distinguish connection failures (increase backoff) from post-auth disconnects (reset backoff to initial).
- **notify_push + webhook can coexist.** Both enabled, cooldown deduplicates. Users can run both for redundancy, or just notify_push if they can't open inbound ports.

### Problems encountered

1. **Leaked Tailscale hostname in public GitHub issue.** Created issue #25 with the Nextcloud server's MagicDNS hostname. Caught by user, deleted issue, recreated as #26 without private details. Added feedback memory to prevent recurrence.
2. **Docker trusted_proxies resolution.** Nextcloud can't resolve Docker container hostnames for proxy trust. Spent several attempts with hostnames and occ config before discovering that `TRUSTED_PROXIES` env var (processed by `reverse-proxy.config.php`) overrides all occ changes, and must use IP/CIDR not hostnames.
3. **Duplicate config key.** Test config had two `logging:` sections — YAML unmarshal error caught immediately.

### Test coverage

| Package | Tests |
|---------|-------|
| notifypush | 16 (12 top-level + 4 subtests) |
| **Total** | **99 tests across 9 packages** |

New tests use `httptest.Server` + `websocket.Accept` for fake WebSocket servers. No sleep-based timing — channel reads with timeouts.

### Lessons

- **Never put infrastructure details in public repos.** Tailscale hostnames, IPs, tailnet names — none of it belongs in public issues, PRs, or commits. GitHub edit history preserves deleted content, so the only fix is deleting the entire issue.
- **Nextcloud Docker trusted_proxies is an env-var-wins system.** The `reverse-proxy.config.php` entrypoint script unconditionally overwrites the `trusted_proxies` array from the `TRUSTED_PROXIES` env var. Any `occ config:system:set` changes are silently overridden on next container start.
- **notify_push latency is significantly better than webhooks.** ~800ms from file upload to push notification vs ~10 seconds for the webhook job queue. Combined with peer notification for local changes, the full sync mesh now operates at 1-2 second latency in all directions.

---

## Session 6 — 2026-04-10: Hunting a CRLF in a haystack (#28)

**Issues:** #28 (degraded sync, all syncs fail)
**Commits:** none — fix was a single file rename outside the repo

### What happened

A user reported that the daemon had been running for 19 hours with `fail_count=248, sync_count=0` — every sync was failing. All four event sources were firing correctly, the webhook server was receiving events, but `nextcloudcmd` exited 1 every single time. Phone screenshots uploaded to the server were not appearing on the local sync host without a manual cron sync — and the manual cron sync was *also* failing.

The investigation took longer than the fix.

### The diagnostic dance

First instinct was to grep the daemon journal for errors, but at INFO level the daemon only logs "starting sync" / "sync complete" / "sync failed". The actual exit code is recorded but the *reason* isn't surfaced. At DEBUG level (which the user had configured), the journal is buried under hundreds of thousands of `[ info nextcloud.sync.discovery ]` lines from nextcloudcmd's stderr — every file in the sync tree gets two lines per sync run.

Second instinct was to run the daemon's exact `nextcloudcmd` invocation manually. Initially I copied the command from the cron fallback script, which uses `--silent`. That suppressed *everything* — the exit code came back as 1 with no output. Removing `--silent` and re-running gave 188k lines of output. Grepping for `error|fatal|warn` returned thousands of matches — all benign.

The breakthrough came from sorting unique warning patterns. Among ~30 distinct warning shapes was one that appeared exactly once:

```
PropagateIgnoreJob ... status BlacklistedError ... "400 Bad Request" PUT
.../<filename>%0D%0A (As-Is)v2.pdf
```

`%0D%0A` is the URL-encoding of `\r\n`. Some file in the local tree had a literal CR+LF sequence inside its filename. The Nextcloud server was rejecting the WebDAV `PUT` because that's not a valid WebDAV path. The status `BlacklistedError` rolled all the way up to the root directory propagator, which set the overall sync status to error, which made `nextcloudcmd` exit 1.

`od -c` on the offending file confirmed it: literal `\r \n` bytes between "Schematic" and "(As-Is)" in the filename. Source unknown — most likely a copy-paste from a Word document or an email subject line that included a soft return.

The fix was a `mv` to replace the CR+LF with a single space. nextcloudcmd then ran cleanly (exit 0), the daemon was restarted, and within two minutes the health endpoint was reporting `status: ok`, `sync_count: 9`, `fail_count: 0`.

### Side discovery

While investigating, I noticed something unsettling: even after the fix, the daemon's `e.logger.Info("sync complete", ...)` line in `engine.go` was *never* appearing in journalctl output, even though the health endpoint's `RecordSync` (called immediately before the log statement) was incrementing correctly. So syncs were definitely succeeding in the engine's view, but the corresponding INFO log line was missing.

The most likely cause: at `level: debug`, the daemon was emitting tens of thousands of `nextcloudcmd stderr` debug lines per sync. systemd-journald has a default rate limit of 10000 messages per 30s per service, after which messages are dropped silently (no `[Suppressed N messages]` notice in the user-mode journal). The INFO lines are presumably being eaten by the same rate limiter that's eating all the chatty DEBUG output.

This is a real daemon design problem — the `level: debug` config makes the daemon's own status events disappear, which is the worst possible failure mode for a debug knob. Filed as a follow-up on #28 along with three other improvements.

### Lessons

- **A `--silent` flag in the diagnostic cron script is the worst possible default for a fallback that's meant to be invoked when the daemon is broken.** The cron sync was both failing and silent, so it gave the user nothing to work with. If a tool is run by automation, its output should still go *somewhere*; the right pattern is `> /var/log/foo.log 2>&1`, never `--silent`.
- **Per-file warnings in nextcloudcmd are visually identical regardless of severity.** A `BlacklistedError` looks like `Conflict` which looks like `FileIgnored`. The only line that tells you which item killed the sync is the final root-directory `slotDirDeletionJobsFinished reporting previous error` — and that scrolls off-screen quickly. The daemon should parse this out and log it.
- **`level: debug` is too coarse a knob.** The daemon needs an intermediate level (or a separate destination for the nextcloudcmd subprocess output) so that engine-level INFO events stay visible when subprocess DEBUG is enabled. This is what `level: trace` would buy you in tracing libraries.
- **Filenames with control characters are real and they break Nextcloud sync silently.** Should be detected and quarantined at the daemon layer — either reject the sync with a useful error, or move the offending file aside automatically. Linux filesystems will happily accept any byte sequence; WebDAV will not.
- **When investigating a degraded service, write diagnostic output to a path *outside* the project directory.** I started writing the manual nextcloudcmd log into `~/nextcloud-sync-daemon/` (the public repo) and only realized when the user asked me to check for leaks. Moved it to `~/nsd-debug/` immediately. No leak, but a near miss — the right default is to use a quarantined scratch directory from the start.
