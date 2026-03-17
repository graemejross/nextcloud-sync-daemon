// Package webhook implements an HTTP server EventSource that receives
// Nextcloud webhook_listeners push events and triggers syncs.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
	"github.com/graemejross/nextcloud-sync-daemon/internal/health"
)

// Server listens for Nextcloud webhook events and sends sync triggers.
type Server struct {
	listen     string
	secret     string
	pathFilter string
	logger     *slog.Logger
	health     *health.Status

	// Per-IP rate limiting
	rateMu      sync.Mutex
	rateMap     map[string]time.Time
	rateMin     time.Duration // minimum interval between requests per IP
	rateCleanup time.Time     // last time stale entries were purged
}

// New creates a webhook Server with per-IP rate limiting (5 seconds between requests per IP).
// The health parameter is optional (may be nil).
func New(listen, secret, pathFilter string, logger *slog.Logger, health *health.Status) *Server {
	return &Server{
		listen:     listen,
		secret:     secret,
		pathFilter: pathFilter,
		logger:     logger,
		health:     health,
		rateMap:    make(map[string]time.Time),
		rateMin:    5 * time.Second,
	}
}

// Start begins the HTTP server. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context, trigger chan<- daemon.Event) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handler(trigger))

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Listen on the configured address
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("webhook listen on %s: %w", s.listen, err)
	}

	s.logger.Info("webhook server started", "listen", s.listen)

	// Shutdown when context is cancelled
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("webhook shutdown error", "error", err)
		}
	}()

	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		s.logger.Info("webhook server stopped")
		return nil
	}
	return err
}

// Name returns the source name for logging.
func (s *Server) Name() string {
	return "webhook"
}

// handler returns the HTTP handler function for webhook requests.
func (s *Server) handler(trigger chan<- daemon.Event) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// GET — health check
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
			return
		}

		// Only accept POST
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Validate secret
		headerSecret := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(headerSecret), []byte(s.secret)) != 1 {
			s.logger.Warn("webhook rejected: invalid secret",
				"remote", r.RemoteAddr,
			)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Record webhook receipt for health reporting
		if s.health != nil {
			s.health.RecordWebhookReceived(time.Now())
		}

		// Per-IP rate limiting
		if s.rateMin > 0 {
			ip := stripPort(r.RemoteAddr)
			now := time.Now()
			s.rateMu.Lock()
			// Purge stale entries every 10 minutes to prevent unbounded growth
			if now.Sub(s.rateCleanup) > 10*time.Minute {
				for k, v := range s.rateMap {
					if now.Sub(v) > s.rateMin {
						delete(s.rateMap, k)
					}
				}
				s.rateCleanup = now
			}
			if last, ok := s.rateMap[ip]; ok && now.Sub(last) < s.rateMin {
				s.rateMu.Unlock()
				s.logger.Debug("webhook rate-limited",
					"remote", r.RemoteAddr,
					"min_interval", s.rateMin,
				)
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, "rate limited")
				return
			}
			s.rateMap[ip] = now
			s.rateMu.Unlock()
		}

		// Read and parse body
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
		if err != nil {
			s.logger.Error("webhook body read error", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Parse payload to extract path for filtering
		path := s.extractPath(body)

		// Apply path filter
		if s.pathFilter != "" && s.pathFilter != "/" && !strings.Contains(path, s.pathFilter) {
			s.logger.Debug("webhook event filtered",
				"path", path,
				"filter", s.pathFilter,
			)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "filtered")
			return
		}

		// Send trigger event
		event := daemon.Event{
			Source: "webhook",
			Time:   time.Now(),
			Path:   path,
		}
		select {
		case trigger <- event:
			s.logger.Debug("webhook event received", "path", path)
		default:
			s.logger.Debug("webhook event dropped (sync already pending)", "path", path)
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}
}

// nextcloudPayload represents the structure of Nextcloud webhook_listeners events.
type nextcloudPayload struct {
	Event struct {
		Class string `json:"class"`
		Node  struct {
			Path string `json:"path"`
		} `json:"node"`
	} `json:"event"`
}

// extractPath parses the Nextcloud webhook payload and returns the file path.
// Returns "unknown" if parsing fails — we still trigger a sync on any valid notification.
func (s *Server) extractPath(body []byte) string {
	var payload nextcloudPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.logger.Debug("webhook payload parse failed", "error", err)
		return "unknown"
	}

	if payload.Event.Node.Path != "" {
		return payload.Event.Node.Path
	}

	return "unknown"
}

// stripPort removes the port from an address like "192.168.1.1:12345" or "[::1]:12345".
func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
