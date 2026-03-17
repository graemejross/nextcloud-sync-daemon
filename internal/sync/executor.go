// Package sync implements the nextcloudcmd executor.
package sync

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
	"github.com/graemejross/nextcloud-sync-daemon/internal/daemon"
)

// Executor runs nextcloudcmd and satisfies daemon.SyncExecutor.
type Executor struct {
	cfg    *config.Config
	logger *slog.Logger
	mu     sync.Mutex
}

// NewExecutor creates a sync executor.
func NewExecutor(cfg *config.Config, logger *slog.Logger) *Executor {
	return &Executor{cfg: cfg, logger: logger}
}

// Run executes a single nextcloudcmd sync. Thread-safe via mutex.
func (e *Executor) Run(ctx context.Context) (*daemon.SyncResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := &daemon.SyncResult{
		StartTime: time.Now(),
	}

	password, err := e.cfg.ResolvePassword()
	if err != nil {
		result.Error = fmt.Errorf("resolving password: %w", err)
		result.Duration = time.Since(result.StartTime)
		result.ExitCode = -1
		return result, result.Error
	}

	args := e.buildArgs(password)

	timeout := e.cfg.Sync.Timeout.Duration
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, e.cfg.Sync.NextcloudCmd, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	e.logger.Info("starting sync",
		"cmd", e.cfg.Sync.NextcloudCmd,
		"local_dir", e.cfg.Sync.LocalDir,
		"remote_path", e.cfg.Sync.RemotePath,
	)

	err = cmd.Run()
	result.Duration = time.Since(result.StartTime)

	if stdout.Len() > 0 {
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			e.logger.Debug("nextcloudcmd stdout", "line", line)
		}
	}
	if stderr.Len() > 0 {
		for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
			e.logger.Debug("nextcloudcmd stderr", "line", line)
		}
	}

	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			result.Error = fmt.Errorf("sync timed out after %v", timeout)
			result.ExitCode = -1
			e.logger.Warn("sync timed out", "timeout", timeout)
			return result, result.Error
		}

		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			result.Error = fmt.Errorf("nextcloudcmd exited with code %d", result.ExitCode)
			e.logger.Error("sync failed",
				"exit_code", result.ExitCode,
				"duration_ms", result.Duration.Milliseconds(),
			)
			return result, nil // non-zero exit is not a Go error
		}

		result.Error = fmt.Errorf("running nextcloudcmd: %w", err)
		result.ExitCode = -1
		return result, result.Error
	}

	result.ExitCode = 0

	return result, nil
}

// buildArgs constructs the nextcloudcmd argument list.
func (e *Executor) buildArgs(password string) []string {
	var args []string

	args = append(args, e.cfg.Sync.ExtraArgs...)
	args = append(args, "--non-interactive")
	args = append(args, "-u", e.cfg.Server.Username)
	args = append(args, "-p", password)
	args = append(args, "--path", e.cfg.Sync.RemotePath)
	args = append(args, e.cfg.Sync.LocalDir)
	args = append(args, e.cfg.Server.URL)

	return args
}

// CheckNextcloudCmd verifies that nextcloudcmd is available.
func CheckNextcloudCmd(path string) error {
	_, err := exec.LookPath(path)
	if err != nil {
		return fmt.Errorf("nextcloudcmd not found at %q: %w", path, err)
	}
	return nil
}
