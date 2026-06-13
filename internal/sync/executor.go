// Package sync implements the nextcloudcmd executor.
package sync

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

	// Authenticate via a temporary netrc rather than -u/-p on the command
	// line (Refs #30): argv is world-readable via /proc/<pid>/cmdline, so a
	// -p password is visible to any local process for the whole sync. The
	// netrc lives 0600 in a 0700 dir and is removed when the sync returns.
	netrcHome, cleanup, err := e.writeNetrc(password)
	if err != nil {
		result.Error = fmt.Errorf("preparing netrc: %w", err)
		result.Duration = time.Since(result.StartTime)
		result.ExitCode = -1
		return result, result.Error
	}
	defer cleanup()

	args := e.buildArgs()

	timeout := e.cfg.Sync.Timeout.Duration
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, e.cfg.Sync.NextcloudCmd, args...)

	// Point HOME at the temp netrc dir so `nextcloudcmd -n` reads our
	// credentials there, not the daemon user's real ~/.netrc.
	cmd.Env = envWithHome(netrcHome)

	// Kernel-level orphan prevention (Refs #27):
	//   Pdeathsig: SIGKILL — Linux kernel sends SIGKILL to this child if the
	//     daemon dies for any reason (including SIGKILL/OOM where Go cannot
	//     run cleanup). Without this, a killed daemon leaves nextcloudcmd
	//     orphaned to PID 1, and the next daemon instance spawns its own
	//     parallel client → thrashing.
	//   Setpgid: true — put nextcloudcmd in its own process group so signals
	//     reach the entire subprocess tree if it spawns children of its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}

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

// buildArgs constructs the nextcloudcmd argument list. Credentials are NOT
// passed here — `-n` makes nextcloudcmd read them from the netrc under the
// HOME we set on the command's environment (Refs #30). Nothing secret, and
// no username, reaches argv.
func (e *Executor) buildArgs() []string {
	var args []string

	args = append(args, e.cfg.Sync.ExtraArgs...)
	args = append(args, "--non-interactive")
	args = append(args, "-n")
	args = append(args, "--path", e.cfg.Sync.RemotePath)
	args = append(args, e.cfg.Sync.LocalDir)
	args = append(args, e.cfg.Server.URL)

	return args
}

// writeNetrc creates a temporary directory (0700) holding a .netrc (0600)
// with the server credentials, for `nextcloudcmd -n`. Returns the directory
// to use as HOME and a cleanup func the caller must defer. Keeping the
// password in a private file instead of on argv is the whole point of #30.
func (e *Executor) writeNetrc(password string) (string, func(), error) {
	u, err := url.Parse(e.cfg.Server.URL)
	if err != nil {
		return "", nil, fmt.Errorf("parsing server URL %q: %w", e.cfg.Server.URL, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", nil, fmt.Errorf("server URL %q has no host", e.cfg.Server.URL)
	}

	dir, err := os.MkdirTemp("", "nsd-netrc-")
	if err != nil {
		return "", nil, fmt.Errorf("creating netrc temp dir: %w", err)
	}
	cleanup := func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			e.logger.Warn("failed to remove netrc temp dir", "dir", dir, "error", rmErr)
		}
	}

	// machine line matches the server host; nextcloudcmd looks up login/password by host.
	content := fmt.Sprintf("machine %s login %s password %s\n", host, e.cfg.Server.Username, password)
	if err := os.WriteFile(filepath.Join(dir, ".netrc"), []byte(content), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing netrc: %w", err)
	}

	return dir, cleanup, nil
}

// envWithHome returns the current environment with HOME replaced by the given
// path, so a child process reads netrc/config from there.
func envWithHome(home string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home)
}

// CheckNextcloudCmd verifies that nextcloudcmd is available.
func CheckNextcloudCmd(path string) error {
	_, err := exec.LookPath(path)
	if err != nil {
		return fmt.Errorf("nextcloudcmd not found at %q: %w", path, err)
	}
	return nil
}
