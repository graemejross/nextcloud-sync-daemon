#!/usr/bin/env python3
"""Layer 2: Webhook listener — receives Nextcloud file change events and triggers sync.

Listens for POST requests from Nextcloud webhook_listeners app.
Only triggers sync for changes matching the configured path filter.

Configuration is read from config.sh (shared with bash scripts).
"""

import json
import os
import subprocess
import sys
from datetime import datetime
from http.server import HTTPServer, BaseHTTPRequestHandler
from pathlib import Path


def load_config():
    """Read config values from config.sh (bash key=value format)."""
    config_path = Path(__file__).resolve().parent.parent / "config.sh"
    if not config_path.exists():
        print(f"ERROR: Config file not found: {config_path}", file=sys.stderr)
        print(
            "Copy config.example to config.sh and edit for your environment.",
            file=sys.stderr,
        )
        sys.exit(1)

    config = {}
    with open(config_path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if "=" in line:
                key, _, value = line.partition("=")
                # Strip quotes and resolve simple ${VAR} references
                value = value.strip().strip('"').strip("'")
                # Resolve references to previously defined config values
                for prev_key, prev_value in config.items():
                    value = value.replace(f"${{{prev_key}}}", prev_value)
                config[key] = value
    return config


CONFIG = load_config()
PORT = int(CONFIG.get("WEBHOOK_PORT", "8767"))
SECRET = CONFIG.get("WEBHOOK_SECRET", "")
PATH_FILTER = CONFIG.get("WEBHOOK_PATH_FILTER", "/")
LOG_DIR = CONFIG.get("SYNC_LOG_DIR", ".")
SYNC_SCRIPT = str(Path(__file__).resolve().parent / "nextcloud-sync.sh")
LOGFILE = Path(LOG_DIR) / "webhook.log"


def log(msg):
    with open(LOGFILE, "a") as f:
        f.write(f"{datetime.now().isoformat()} {msg}\n")


class WebhookHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        # Check shared secret
        auth = self.headers.get("X-Webhook-Secret", "")
        if auth != SECRET:
            log(f"Rejected: bad secret from {self.client_address[0]}")
            self.send_response(403)
            self.end_headers()
            return

        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length).decode("utf-8", errors="replace")

        try:
            data = json.loads(body) if body else {}
        except json.JSONDecodeError:
            data = {}

        # Extract path from event payload
        event_class = data.get("event", {}).get("class", "unknown")
        node = data.get("event", {}).get("node", {})
        path = node.get("path", "unknown")

        log(f"Received {event_class}: {path}")

        # Sync for changes matching the configured path filter
        if PATH_FILTER in path:
            log("Triggering sync")
            try:
                subprocess.Popen(
                    [SYNC_SCRIPT],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
            except Exception as e:
                log(f"Sync trigger failed: {e}")
        else:
            log(f"Ignoring — not in {PATH_FILTER}: {path}")

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_GET(self):
        """Health check endpoint."""
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"nextcloud-webhook-listener ok")

    def log_message(self, format, *args):
        # Suppress default stderr logging
        pass


if __name__ == "__main__":
    log(f"Starting webhook listener on port {PORT}")
    server = HTTPServer(("0.0.0.0", PORT), WebhookHandler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log("Shutting down")
        server.server_close()
