package appauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer implements the two Login Flow v2 endpoints. approveAfter is the
// number of 404 polls before the credentials are released.
func fakeServer(t *testing.T, approveAfter int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/index.php/login/v2", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"poll": map[string]string{
				"token":    "test-token",
				"endpoint": srv.URL + "/login/v2/poll",
			},
			"login": srv.URL + "/login/v2/flow/abc",
		})
	})

	mux.HandleFunc("/login/v2/poll", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("token") != "test-token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if polls.Add(1) <= approveAfter {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(Credentials{
			Server:      srv.URL,
			LoginName:   "sync-user",
			AppPassword: "generated-app-password",
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestStartParsesFlow(t *testing.T) {
	srv, _ := fakeServer(t, 0)

	f, err := Start(context.Background(), srv.Client(), srv.URL+"/", "nextcloud-sync-daemon test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if f.LoginURL != srv.URL+"/login/v2/flow/abc" {
		t.Errorf("LoginURL = %s", f.LoginURL)
	}
	if f.pollEndpoint != srv.URL+"/login/v2/poll" || f.pollToken != "test-token" {
		t.Errorf("poll = %s token = %s", f.pollEndpoint, f.pollToken)
	}
}

func TestStartRejectsNonNextcloud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	_, err := Start(context.Background(), srv.Client(), srv.URL, "ua")
	if err == nil {
		t.Fatal("expected error for empty flow response")
	}
}

func TestStartSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not here", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := Start(context.Background(), srv.Client(), srv.URL, "ua")
	if err == nil {
		t.Fatal("expected error for 404 on flow start")
	}
}

func TestPollWaitsForApproval(t *testing.T) {
	srv, polls := fakeServer(t, 3)

	f, err := Start(context.Background(), srv.Client(), srv.URL, "ua")
	if err != nil {
		t.Fatal(err)
	}

	creds, err := f.Poll(context.Background(), srv.Client(), time.Millisecond)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if creds.AppPassword != "generated-app-password" || creds.LoginName != "sync-user" {
		t.Errorf("creds = %+v", creds)
	}
	if got := polls.Load(); got < 4 {
		t.Errorf("expected at least 4 polls (3 pending + 1 approved), got %d", got)
	}
}

func TestPollHonoursContextCancel(t *testing.T) {
	srv, _ := fakeServer(t, 1<<30) // never approves

	f, err := Start(context.Background(), srv.Client(), srv.URL, "ua")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = f.Poll(ctx, srv.Client(), time.Millisecond)
	if err == nil {
		t.Fatal("expected context error")
	}
}
