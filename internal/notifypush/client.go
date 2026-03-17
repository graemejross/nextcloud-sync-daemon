// Package notifypush implements an EventSource that connects to Nextcloud's
// notify_push app via WebSocket for near-real-time server-side file change events.
// Unlike the webhook source, this connects outbound — no inbound ports needed.
package notifypush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/health"
)

// Client connects to a Nextcloud notify_push WebSocket endpoint and emits
// sync events when file changes are reported by the server.
type Client struct {
	serverURL         string
	pushURL           string // discovered or configured
	username          string
	resolvePassword   func() (string, error)
	reconnectInterval time.Duration
	reconnectMax      time.Duration
	logger            *slog.Logger
	health            *health.Status
}

// New creates a notify_push Client from the daemon config.
func New(cfg *config.Config, logger *slog.Logger, health *health.Status) *Client {
	return &Client{
		serverURL:         cfg.Server.URL,
		pushURL:           cfg.NotifyPush.URL,
		username:          cfg.Server.Username,
		resolvePassword:   cfg.ResolvePassword,
		reconnectInterval: cfg.NotifyPush.ReconnectInterval.Duration,
		reconnectMax:      cfg.NotifyPush.ReconnectMax.Duration,
		logger:            logger.With("source", "notify_push"),
		health:            health,
	}
}

// Start connects to the notify_push WebSocket and emits events on the trigger channel.
// Blocks until ctx is cancelled, reconnecting with exponential backoff on failure.
func (c *Client) Start(ctx context.Context, trigger chan<- daemon.Event) error {
	c.logger.Info("notify_push client starting")
	backoff := c.reconnectInterval

	for {
		wasAuthenticated, err := c.connectAndListen(ctx, trigger)
		if ctx.Err() != nil {
			c.logger.Info("notify_push client stopped")
			return nil
		}

		c.logger.Warn("notify_push disconnected, reconnecting",
			"error", err,
			"backoff", backoff,
		)

		select {
		case <-ctx.Done():
			c.logger.Info("notify_push client stopped")
			return nil
		case <-time.After(backoff):
		}

		if wasAuthenticated {
			// Reset backoff after successful connections — the disconnect was likely transient
			backoff = c.reconnectInterval
		} else {
			// Exponential backoff for connection/auth failures
			backoff = backoff * 2
			if backoff > c.reconnectMax {
				backoff = c.reconnectMax
			}
		}
	}
}

// Name returns the event source name.
func (c *Client) Name() string {
	return "notify_push"
}

// connectAndListen handles a single WebSocket connection lifecycle:
// discover URL, connect, authenticate, and read messages until disconnect.
// Returns whether authentication succeeded (for backoff reset) and any error.
func (c *Client) connectAndListen(ctx context.Context, trigger chan<- daemon.Event) (authenticated bool, err error) {
	// Discover push URL if not configured
	pushURL := c.pushURL
	if pushURL == "" {
		pushURL, err = c.discoverPushURL(ctx)
		if err != nil {
			return false, fmt.Errorf("discovering push URL: %w", err)
		}
		c.logger.Info("discovered notify_push endpoint", "url", pushURL)
	}

	// Connect
	conn, _, err := websocket.Dial(ctx, pushURL, nil)
	if err != nil {
		return false, fmt.Errorf("connecting to %s: %w", pushURL, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutting down")

	// Authenticate
	if err := c.authenticate(ctx, conn); err != nil {
		return false, fmt.Errorf("authentication: %w", err)
	}

	c.logger.Info("notify_push connected and authenticated", "url", pushURL)

	// Start ping goroutine
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go c.pingLoop(pingCtx, conn)

	// Read messages — any error here is post-auth, so return authenticated=true
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return true, fmt.Errorf("reading message: %w", err)
		}

		c.handleMessage(string(msg), trigger)
	}
}

// authenticate sends username and password, then waits for "authenticated".
func (c *Client) authenticate(ctx context.Context, conn *websocket.Conn) error {
	// Send username
	if err := conn.Write(ctx, websocket.MessageText, []byte(c.username)); err != nil {
		return fmt.Errorf("sending username: %w", err)
	}

	// Send password
	password, err := c.resolvePassword()
	if err != nil {
		return fmt.Errorf("resolving password: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(password)); err != nil {
		return fmt.Errorf("sending password: %w", err)
	}

	// Read response
	_, msg, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading auth response: %w", err)
	}

	response := string(msg)
	if response != "authenticated" {
		return fmt.Errorf("server rejected credentials: %s", response)
	}

	return nil
}

// discoverPushURL queries the Nextcloud capabilities API to find the WebSocket endpoint.
func (c *Client) discoverPushURL(ctx context.Context) (string, error) {
	password, err := c.resolvePassword()
	if err != nil {
		return "", fmt.Errorf("resolving password: %w", err)
	}

	capURL := strings.TrimRight(c.serverURL, "/") + "/ocs/v2.php/cloud/capabilities?format=json"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, capURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(c.username, password)
	req.Header.Set("OCS-APIRequest", "true")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting capabilities: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("capabilities returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading capabilities response: %w", err)
	}

	// Parse the nested JSON: ocs.data.capabilities.notify_push.endpoints.websocket
	var result capabilitiesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing capabilities JSON: %w", err)
	}

	wsURL := result.OCS.Data.Capabilities.NotifyPush.Endpoints.WebSocket
	if wsURL == "" {
		return "", fmt.Errorf("notify_push not found in server capabilities (is the app installed and enabled?)")
	}

	return wsURL, nil
}

// capabilitiesResponse models the relevant portion of the Nextcloud capabilities API response.
type capabilitiesResponse struct {
	OCS struct {
		Data struct {
			Capabilities struct {
				NotifyPush struct {
					Endpoints struct {
						WebSocket string `json:"websocket"`
					} `json:"endpoints"`
				} `json:"notify_push"`
			} `json:"capabilities"`
		} `json:"data"`
	} `json:"ocs"`
}

// handleMessage processes a single message from the WebSocket.
func (c *Client) handleMessage(msg string, trigger chan<- daemon.Event) {
	switch {
	case msg == "notify_file" || strings.HasPrefix(msg, "notify_file_id"):
		c.logger.Debug("file change notification received", "message", msg)
		event := daemon.Event{
			Source: "notify_push",
			Time:   time.Now(),
		}
		select {
		case trigger <- event:
			c.logger.Debug("notify_push event sent")
		default:
			c.logger.Debug("notify_push event dropped (sync already pending)")
		}
	case msg == "notify_activity", msg == "notify_notification":
		c.logger.Debug("ignoring non-file notification", "message", msg)
	default:
		c.logger.Debug("unknown push message", "message", msg)
	}
}

// pingLoop sends WebSocket pings every 30 seconds to keep the connection alive.
func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.Ping(ctx); err != nil {
				c.logger.Debug("ping failed", "error", err)
				return
			}
		}
	}
}
