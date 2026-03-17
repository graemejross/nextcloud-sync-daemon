// Package peer implements notifications to peer sync daemon instances.
package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/health"
)

// Notifier sends webhook-style POST requests to peer instances after local syncs.
type Notifier struct {
	peers  []config.PeerConfig
	client *http.Client
	logger *slog.Logger
	health *health.Status
}

type notification struct {
	Source string `json:"source"`
	Time   string `json:"time"`
}

// New creates a Notifier for the given peers. The health parameter is optional (may be nil).
func New(peers []config.PeerConfig, logger *slog.Logger, health *health.Status) *Notifier {
	return &Notifier{
		peers: peers,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: logger,
		health: health,
	}
}

// NotifyPeers sends a notification POST to all configured peers concurrently.
// Errors are logged but never returned — peer failures must not block the sync loop.
func (n *Notifier) NotifyPeers(ctx context.Context) {
	if len(n.peers) == 0 {
		return
	}

	payload, err := json.Marshal(notification{
		Source: "peer",
		Time:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		n.logger.Error("failed to marshal peer notification", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, p := range n.peers {
		wg.Add(1)
		go func(peer config.PeerConfig) {
			defer wg.Done()
			n.notifyPeer(ctx, peer, payload)
		}(p)
	}
	wg.Wait()
}

func (n *Notifier) notifyPeer(ctx context.Context, peer config.PeerConfig, payload []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.URL, bytes.NewReader(payload))
	if err != nil {
		n.logger.Warn("failed to create peer request", "url", peer.URL, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", peer.Secret)

	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.Warn("peer notification failed", "url", peer.URL, "error", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		n.logger.Debug("peer notified", "url", peer.URL, "status", resp.StatusCode)
	} else {
		n.logger.Warn("peer notification rejected", "url", peer.URL, "status", resp.StatusCode)
	}
}
