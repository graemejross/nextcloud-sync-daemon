#!/bin/bash
# Layer 1: inotifywait — watch local sync directory for changes.
# Triggers nextcloud-sync.sh when files are created, modified, deleted, or moved.
# Run as systemd service: nextcloud-watch.service

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG="${SCRIPT_DIR}/../config.sh"

if [ ! -f "$CONFIG" ]; then
    echo "ERROR: Config file not found: $CONFIG" >&2
    echo "Copy config.example to config.sh and edit for your environment." >&2
    exit 1
fi
source "$CONFIG"

SYNC_SCRIPT="${SCRIPT_DIR}/nextcloud-sync.sh"
LOGFILE="${SYNC_LOG_DIR}/watch.log"
LAST_SYNC_FILE="/tmp/nextcloud-watch-last-sync"

log() {
    echo "$(date -Is) $1" >> "$LOGFILE"
}

log "Watcher started on $SYNC_LOCAL_DIR (debounce=${WATCH_DEBOUNCE}s, cooldown=${WATCH_COOLDOWN}s)"

# Monitor recursively for file changes
# --monitor: keep running after events
# --recursive: watch subdirectories
# --event: only these event types
# --exclude: skip sync database files
inotifywait --monitor --recursive \
    --event close_write,create,delete,move \
    --exclude '\.sync_.*\.db' \
    --format '%T %e %w%f' --timefmt '%Y-%m-%dT%H:%M:%S' \
    "$SYNC_LOCAL_DIR" 2>/dev/null | while read -r line; do
    log "Changed: $line"

    # Debounce: drain events for WATCH_DEBOUNCE seconds, then sync once
    while read -t "$WATCH_DEBOUNCE" -r extra; do
        log "Changed: $extra"
    done

    # Cooldown: skip if we synced recently (file-based to survive subshell)
    NOW=$(date +%s)
    LAST_SYNC=$(cat "$LAST_SYNC_FILE" 2>/dev/null || echo 0)
    ELAPSED=$(( NOW - LAST_SYNC ))
    if [ "$ELAPSED" -lt "$WATCH_COOLDOWN" ]; then
        printf "." >> "$LOGFILE"
        continue
    fi

    # Newline to close any trailing dots from skipped syncs
    printf "\n" >> "$LOGFILE"
    log "Triggering sync"
    "$SYNC_SCRIPT" 2>&1 || log "Sync failed with exit $?"
    date +%s > "$LAST_SYNC_FILE"
    log "Sync triggered by inotify complete"
done
