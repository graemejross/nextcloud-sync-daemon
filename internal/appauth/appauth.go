// Package appauth obtains a Nextcloud app password via Login Flow v2
// (--get-app-password). Refs #37.
//
// The flow is the one the desktop client uses, designed for headless
// clients: the CLI POSTs to /index.php/login/v2 and receives a login URL
// plus a poll token. The user opens the login URL in a browser on any
// device — never this machine — and approves. The CLI polls until the
// server releases the credentials, then prints the app password.
//
// See https://docs.nextcloud.com/server/latest/developer_manual/client_apis/LoginFlow/
package appauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Flow is a started Login Flow v2 session.
type Flow struct {
	// LoginURL is opened by the user in a browser on any device.
	LoginURL string
	// pollEndpoint + pollToken drive the poll loop.
	pollEndpoint string
	pollToken    string
}

// Credentials is the result of an approved login flow.
type Credentials struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// tokenValidity is how long the server keeps the poll token alive.
const tokenValidity = 20 * time.Minute

// Start begins a Login Flow v2 session against serverURL. userAgent names
// the resulting app password in the Nextcloud security settings.
func Start(ctx context.Context, client *http.Client, serverURL, userAgent string) (*Flow, error) {
	endpoint := strings.TrimRight(serverURL, "/") + "/index.php/login/v2"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("starting login flow: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("starting login flow: server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Poll struct {
			Token    string `json:"token"`
			Endpoint string `json:"endpoint"`
		} `json:"poll"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parsing login flow response: %w", err)
	}
	if parsed.Login == "" || parsed.Poll.Endpoint == "" || parsed.Poll.Token == "" {
		return nil, errors.New("login flow response missing login URL or poll endpoint — is this a Nextcloud server?")
	}

	return &Flow{
		LoginURL:     parsed.Login,
		pollEndpoint: parsed.Poll.Endpoint,
		pollToken:    parsed.Poll.Token,
	}, nil
}

// Poll waits for the user to approve the login in their browser. It polls
// every interval until the server releases the credentials, the token
// expires (20 minutes), or ctx is cancelled. The server answers 404 until
// approval, then 200 exactly once.
func (f *Flow) Poll(ctx context.Context, client *http.Client, interval time.Duration) (*Credentials, error) {
	deadline := time.Now().Add(tokenValidity)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			return nil, errors.New("login not approved within 20 minutes — the token has expired, run the command again")
		}

		creds, done, err := f.pollOnce(ctx, client)
		if err != nil {
			return nil, err
		}
		if done {
			return creds, nil
		}
	}
}

// pollOnce performs a single poll. done=false means not yet approved.
func (f *Flow) pollOnce(ctx context.Context, client *http.Client) (*Credentials, bool, error) {
	form := url.Values{"token": {f.pollToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.pollEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		// Transient network failure — keep polling rather than aborting a
		// flow the user may be mid-way through approving.
		return nil, false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, false, nil // not approved yet
	case http.StatusOK:
		var creds Credentials
		if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
			return nil, false, fmt.Errorf("parsing credentials: %w", err)
		}
		if creds.AppPassword == "" {
			return nil, false, errors.New("server returned 200 but no app password")
		}
		return &creds, true, nil
	default:
		return nil, false, fmt.Errorf("poll endpoint returned %s", resp.Status)
	}
}
