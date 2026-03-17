package notifypush

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeNotifyPushServer creates a test server that implements the notify_push WebSocket protocol.
// The handler func receives the accepted WebSocket connection for custom behavior.
func fakeNotifyPushServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("websocket accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		handler(conn)
	}))
}

// fakeCapabilitiesServer creates a test server that returns capabilities JSON.
func fakeCapabilitiesServer(t *testing.T, wsURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocs/v2.php/cloud/capabilities" {
			http.NotFound(w, r)
			return
		}
		resp := capabilitiesResponse{}
		resp.OCS.Data.Capabilities.NotifyPush.Endpoints.WebSocket = wsURL
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// testClient creates a Client with test defaults.
func testClient(serverURL, pushURL string) *Client {
	return &Client{
		serverURL:         serverURL,
		pushURL:           pushURL,
		username:          "testuser",
		resolvePassword:   func() (string, error) { return "testpass", nil },
		reconnectInterval: 50 * time.Millisecond,
		reconnectMax:      200 * time.Millisecond,
		logger:            quietLogger(),
	}
}

func TestDiscoverPushURL(t *testing.T) {
	wantURL := "wss://cloud.example.com/push/ws"
	capSrv := fakeCapabilitiesServer(t, wantURL)
	defer capSrv.Close()

	c := testClient(capSrv.URL, "")

	got, err := c.discoverPushURL(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantURL {
		t.Errorf("got %q, want %q", got, wantURL)
	}
}

func TestDiscoverPushURLMissing(t *testing.T) {
	// Server returns capabilities without notify_push
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ocs":{"data":{"capabilities":{}}}}`)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "")

	_, err := c.discoverPushURL(context.Background())
	if err == nil {
		t.Fatal("expected error for missing notify_push")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want to contain 'not found'", err.Error())
	}
}

func TestAuthentication(t *testing.T) {
	var gotUser, gotPass string

	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()

		// Read username
		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Logf("read username error: %v", err)
			return
		}
		gotUser = string(msg)

		// Read password
		_, msg, err = conn.Read(ctx)
		if err != nil {
			t.Logf("read password error: %v", err)
			return
		}
		gotPass = string(msg)

		// Send authenticated
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))

		// Keep connection open until client disconnects
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	trigger := make(chan daemon.Event, 10)
	go c.Start(ctx, trigger)

	// Wait for auth to complete
	time.Sleep(200 * time.Millisecond)
	cancel()

	if gotUser != "testuser" {
		t.Errorf("username = %q, want %q", gotUser, "testuser")
	}
	if gotPass != "testpass" {
		t.Errorf("password = %q, want %q", gotPass, "testpass")
	}
}

func TestAuthenticationFailed(t *testing.T) {
	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		// Read username + password
		conn.Read(ctx)
		conn.Read(ctx)
		// Reject
		conn.Write(ctx, websocket.MessageText, []byte("err: Invalid credentials"))
		conn.Close(websocket.StatusNormalClosure, "")
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Start should reconnect (not return immediately) since ctx is still active.
	// We check that no events are emitted.
	go c.Start(ctx, trigger)

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-trigger:
		t.Error("no event should be sent on auth failure")
	default:
		// good
	}
}

func TestNotifyFileEvent(t *testing.T) {
	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		// Auth handshake
		conn.Read(ctx)
		conn.Read(ctx)
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))

		// Send file notification
		conn.Write(ctx, websocket.MessageText, []byte("notify_file"))

		// Keep alive
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go c.Start(ctx, trigger)

	select {
	case event := <-trigger:
		if event.Source != "notify_push" {
			t.Errorf("source = %q, want %q", event.Source, "notify_push")
		}
		if event.Time.IsZero() {
			t.Error("event time should not be zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within timeout")
	}
}

func TestNotifyFileIdEvent(t *testing.T) {
	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		conn.Read(ctx)
		conn.Read(ctx)
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))
		conn.Write(ctx, websocket.MessageText, []byte("notify_file_id [1,2,3]"))
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go c.Start(ctx, trigger)

	select {
	case event := <-trigger:
		if event.Source != "notify_push" {
			t.Errorf("source = %q, want %q", event.Source, "notify_push")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within timeout")
	}
}

func TestIgnoredEvents(t *testing.T) {
	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		conn.Read(ctx)
		conn.Read(ctx)
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))

		// Send non-file notifications
		conn.Write(ctx, websocket.MessageText, []byte("notify_activity"))
		conn.Write(ctx, websocket.MessageText, []byte("notify_notification"))

		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go c.Start(ctx, trigger)

	// Wait for messages to be processed
	time.Sleep(300 * time.Millisecond)

	select {
	case event := <-trigger:
		t.Errorf("unexpected event: %+v", event)
	default:
		// good — no events for activity/notification messages
	}
}

func TestReconnectOnDisconnect(t *testing.T) {
	connectCount := 0

	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		connectCount++

		conn.Read(ctx)
		conn.Read(ctx)
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))

		if connectCount == 1 {
			// First connection: close immediately after auth
			conn.Close(websocket.StatusGoingAway, "server restart")
			return
		}

		// Second connection: send a file event then stay alive
		conn.Write(ctx, websocket.MessageText, []byte("notify_file"))
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go c.Start(ctx, trigger)

	// Should get an event from the second connection after reconnect
	select {
	case event := <-trigger:
		if event.Source != "notify_push" {
			t.Errorf("source = %q, want %q", event.Source, "notify_push")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event received — reconnect may have failed")
	}

	if connectCount < 2 {
		t.Errorf("connect count = %d, want >= 2", connectCount)
	}
}

func TestContextCancellation(t *testing.T) {
	wsSrv := fakeNotifyPushServer(t, func(conn *websocket.Conn) {
		ctx := context.Background()
		conn.Read(ctx)
		conn.Read(ctx)
		conn.Write(ctx, websocket.MessageText, []byte("authenticated"))
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	})
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	c := testClient("", wsURL)

	trigger := make(chan daemon.Event, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- c.Start(ctx, trigger)
	}()

	// Wait for connection to establish
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within timeout")
	}
}

func TestName(t *testing.T) {
	c := &Client{}
	if c.Name() != "notify_push" {
		t.Errorf("name = %q, want %q", c.Name(), "notify_push")
	}
}

func TestNewFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.URL = "https://cloud.example.com"
	cfg.Server.Username = "alice"
	cfg.Server.Password = "secret"
	cfg.NotifyPush.URL = "wss://cloud.example.com/push/ws"
	cfg.NotifyPush.ReconnectInterval = config.Duration{Duration: 10 * time.Second}
	cfg.NotifyPush.ReconnectMax = config.Duration{Duration: 10 * time.Minute}

	c := New(cfg, quietLogger(), nil)

	if c.serverURL != "https://cloud.example.com" {
		t.Errorf("serverURL = %q", c.serverURL)
	}
	if c.pushURL != "wss://cloud.example.com/push/ws" {
		t.Errorf("pushURL = %q", c.pushURL)
	}
	if c.username != "alice" {
		t.Errorf("username = %q", c.username)
	}
	if c.reconnectInterval != 10*time.Second {
		t.Errorf("reconnectInterval = %v", c.reconnectInterval)
	}
	if c.reconnectMax != 10*time.Minute {
		t.Errorf("reconnectMax = %v", c.reconnectMax)
	}

	// Verify password resolution works
	pw, err := c.resolvePassword()
	if err != nil {
		t.Fatalf("resolvePassword error: %v", err)
	}
	if pw != "secret" {
		t.Errorf("password = %q, want %q", pw, "secret")
	}
}

func TestHandleMessage(t *testing.T) {
	c := testClient("", "")
	trigger := make(chan daemon.Event, 10)

	tests := []struct {
		name      string
		msg       string
		wantEvent bool
	}{
		{"notify_file", "notify_file", true},
		{"notify_file_id", "notify_file_id [42]", true},
		{"notify_activity", "notify_activity", false},
		{"notify_notification", "notify_notification", false},
		{"unknown", "some_other_message", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Drain any leftover events
			for len(trigger) > 0 {
				<-trigger
			}

			c.handleMessage(tt.msg, trigger)

			if tt.wantEvent {
				select {
				case event := <-trigger:
					if event.Source != "notify_push" {
						t.Errorf("source = %q, want %q", event.Source, "notify_push")
					}
				default:
					t.Error("expected event but none sent")
				}
			} else {
				select {
				case event := <-trigger:
					t.Errorf("unexpected event: %+v", event)
				default:
					// good
				}
			}
		})
	}
}
