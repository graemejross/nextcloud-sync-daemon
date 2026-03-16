// Package watcher implements a filesystem EventSource using fsnotify.
// It monitors the local sync directory recursively, debounces rapid changes,
// and filters excluded file patterns.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

// Watcher monitors a directory tree for filesystem changes and sends
// debounced sync trigger events.
type Watcher struct {
	dir      string
	debounce time.Duration
	excludes []*regexp.Regexp
	logger   *slog.Logger
}

// New creates a Watcher for the given directory with compiled exclude patterns.
func New(dir string, debounce time.Duration, excludePatterns []string, logger *slog.Logger) (*Watcher, error) {
	var excludes []*regexp.Regexp
	for _, pattern := range excludePatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", pattern, err)
		}
		excludes = append(excludes, re)
	}

	return &Watcher{
		dir:      dir,
		debounce: debounce,
		excludes: excludes,
		logger:   logger,
	}, nil
}

// Start begins watching the directory tree. Blocks until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context, trigger chan<- daemon.Event) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating fsnotify watcher: %w", err)
	}
	defer fsw.Close()

	// Add initial directory tree
	if err := w.addRecursive(fsw, w.dir); err != nil {
		return fmt.Errorf("watching directory %s: %w", w.dir, err)
	}

	w.logger.Info("watcher started", "dir", w.dir, "debounce", w.debounce)

	// Timer-reset debounce: first event starts timer, subsequent events reset it
	var debounceTimer *time.Timer
	var debounceC <-chan time.Time // nil until timer is active

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			w.logger.Info("watcher stopped")
			return nil

		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}

			// Filter excluded patterns
			relPath, err := filepath.Rel(w.dir, event.Name)
			if err != nil {
				relPath = event.Name
			}
			if w.isExcluded(relPath) {
				w.logger.Debug("event excluded", "path", relPath)
				continue
			}

			w.logger.Debug("filesystem event",
				"op", event.Op.String(),
				"path", relPath,
			)

			// Watch new directories recursively
			if event.Op.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := w.addRecursive(fsw, event.Name); err != nil {
						w.logger.Warn("failed to watch new directory",
							"dir", event.Name,
							"error", err,
						)
					}
				}
			}

			// Reset debounce timer
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(w.debounce)
				debounceC = debounceTimer.C
			} else {
				if !debounceTimer.Stop() {
					// Drain the channel if the timer already fired
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(w.debounce)
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("fsnotify error", "error", err)

		case <-debounceC:
			// Debounce window expired — send event
			event := daemon.Event{
				Source: "watcher",
				Time:   time.Now(),
			}
			select {
			case trigger <- event:
				w.logger.Debug("watcher event sent")
			default:
				w.logger.Debug("watcher event dropped (sync already pending)")
			}
			// Reset for next debounce cycle
			debounceTimer = nil
			debounceC = nil
		}
	}
}

// Name returns the source name for logging.
func (w *Watcher) Name() string {
	return "watcher"
}

// addRecursive adds watches for a directory and all its subdirectories.
func (w *Watcher) addRecursive(fsw *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			relPath, relErr := filepath.Rel(w.dir, path)
			if relErr != nil {
				relPath = path
			}
			if w.isExcluded(relPath) {
				return filepath.SkipDir
			}
			if err := fsw.Add(path); err != nil {
				w.logger.Warn("failed to watch directory", "dir", path, "error", err)
				return nil // continue watching other directories
			}
			w.logger.Debug("watching directory", "dir", path)
		}
		return nil
	})
}

// isExcluded checks if a relative path matches any exclude pattern.
func (w *Watcher) isExcluded(relPath string) bool {
	for _, re := range w.excludes {
		if re.MatchString(relPath) {
			return true
		}
	}
	return false
}
