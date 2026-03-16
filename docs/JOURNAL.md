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

1. **Go not installed.** clarence didn't have Go. Installed 1.22.12 to `/usr/local/go/`.

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
