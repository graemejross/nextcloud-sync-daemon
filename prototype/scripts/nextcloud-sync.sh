#!/bin/bash
# Nextcloud CLI sync — runs nextcloudcmd with lock file protection.
# Called by all three layers (watcher, webhook, polling timer).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG="${SCRIPT_DIR}/../config.sh"

if [ ! -f "$CONFIG" ]; then
    echo "ERROR: Config file not found: $CONFIG" >&2
    echo "Copy config.example to config.sh and edit for your environment." >&2
    exit 1
fi
source "$CONFIG"

LOCKFILE="/tmp/nextcloud-sync.lock"
LOGFILE="${SYNC_LOG_DIR}/sync.log"

# Prevent overlapping runs
if [ -f "$LOCKFILE" ]; then
    pid=$(cat "$LOCKFILE" 2>/dev/null)
    if kill -0 "$pid" 2>/dev/null; then
        echo "$(date -Is) Sync already running (PID $pid), skipping" >> "$LOGFILE"
        exit 0
    fi
    # Stale lock file — remove it
    rm -f "$LOCKFILE"
fi

echo $$ > "$LOCKFILE"
trap 'rm -f "$LOCKFILE"' EXIT

# Load credentials (NEXTCLOUD_SYNC_USER, NEXTCLOUD_SYNC_APP_PASSWORD, NEXTCLOUD_SYNC_URL)
source "$SYNC_CREDENTIALS"

# Run sync
echo "$(date -Is) Starting sync" >> "$LOGFILE"
nextcloudcmd --non-interactive --silent \
    -u "$NEXTCLOUD_SYNC_USER" \
    -p "$NEXTCLOUD_SYNC_APP_PASSWORD" \
    --path "$SYNC_REMOTE_PATH" \
    "$SYNC_LOCAL_DIR" \
    "$NEXTCLOUD_SYNC_URL" \
    >> "$LOGFILE" 2>&1

echo "$(date -Is) Sync complete" >> "$LOGFILE"
