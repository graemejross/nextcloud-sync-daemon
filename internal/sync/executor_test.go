package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/graemejross/nextcloud-sync-daemon/internal/config"
)

// TestHelperProcess is the subprocess entry point for the TestHelperProcess pattern.
// When GO_TEST_HELPER_PROCESS=1, it acts as a mock nextcloudcmd.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER_PROCESS") != "1" {
		return
	}

	exitCode := 0
	if code := os.Getenv("GO_TEST_HELPER_EXIT_CODE"); code != "" {
		var err error
		exitCode, err = strconv.Atoi(code)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid exit code: %s\n", code)
			os.Exit(2)
		}
	}

	if os.Getenv("GO_TEST_HELPER_PRINT_ARGS") == "1" {
		// Skip test framework args, print only our args (after --)
		args := os.Args
		for i, a := range args {
			if a == "--" {
				args = args[i+1:]
				break
			}
		}
		fmt.Println(strings.Join(args, "\n"))
	}

	if delay := os.Getenv("GO_TEST_HELPER_DELAY"); delay != "" {
		d, _ := time.ParseDuration(delay)
		time.Sleep(d)
	}

	os.Exit(exitCode)
}

// fakeNextcloudCmd returns a path to a script that invokes the test binary
// as a helper process. This is the standard Go TestHelperProcess pattern.
func fakeNextcloudCmd(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "nextcloudcmd")

	// Create a shell script that calls the test binary with the helper process env
	content := fmt.Sprintf(`#!/bin/sh
exec %s -test.run=TestHelperProcess -- "$@"
`, os.Args[0])

	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	return script
}

func testConfig(t *testing.T, nextcloudcmd string) *config.Config {
	t.Helper()
	return &config.Config{
		Server: config.ServerConfig{
			URL:      "https://cloud.example.com",
			Username: "alice",
			Password: "secret123",
		},
		Sync: config.SyncConfig{
			LocalDir:     "/home/alice/nextcloud",
			RemotePath:   "/alice",
			NextcloudCmd: nextcloudcmd,
			Timeout:      config.Duration{Duration: 30 * time.Second},
		},
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestExecutorSuccess(t *testing.T) {
	cmd := fakeNextcloudCmd(t)
	cfg := testConfig(t, cmd)

	t.Setenv("GO_TEST_HELPER_PROCESS", "1")
	t.Setenv("GO_TEST_HELPER_EXIT_CODE", "0")
	t.Setenv("GO_TEST_HELPER_PRINT_ARGS", "0")

	executor := NewExecutor(cfg, quietLogger())
	result, err := executor.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Duration <= 0 {
		t.Error("duration should be positive")
	}
}

func TestExecutorNonZeroExit(t *testing.T) {
	cmd := fakeNextcloudCmd(t)
	cfg := testConfig(t, cmd)

	t.Setenv("GO_TEST_HELPER_PROCESS", "1")
	t.Setenv("GO_TEST_HELPER_EXIT_CODE", "1")
	t.Setenv("GO_TEST_HELPER_PRINT_ARGS", "0")

	executor := NewExecutor(cfg, quietLogger())
	result, err := executor.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", result.ExitCode)
	}
	if result.Error == nil {
		t.Error("expected result.Error for non-zero exit")
	}
}

func TestExecutorTimeout(t *testing.T) {
	cmd := fakeNextcloudCmd(t)
	cfg := testConfig(t, cmd)
	cfg.Sync.Timeout = config.Duration{Duration: 200 * time.Millisecond}

	t.Setenv("GO_TEST_HELPER_PROCESS", "1")
	t.Setenv("GO_TEST_HELPER_EXIT_CODE", "0")
	t.Setenv("GO_TEST_HELPER_PRINT_ARGS", "0")
	t.Setenv("GO_TEST_HELPER_DELAY", "5s")

	executor := NewExecutor(cfg, quietLogger())
	result, err := executor.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for timeout")
	}
	if result.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.Error.Error(), "timed out") {
		t.Errorf("error = %q, want to contain 'timed out'", result.Error.Error())
	}
}

func TestExecutorPasswordFromFile(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "password")
	if err := os.WriteFile(pwFile, []byte("file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := fakeNextcloudCmd(t)
	cfg := testConfig(t, cmd)
	cfg.Server.Password = ""
	cfg.Server.PasswordFile = pwFile

	t.Setenv("GO_TEST_HELPER_PROCESS", "1")
	t.Setenv("GO_TEST_HELPER_EXIT_CODE", "0")
	t.Setenv("GO_TEST_HELPER_PRINT_ARGS", "0")

	executor := NewExecutor(cfg, quietLogger())
	result, err := executor.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestExecutorPasswordFileMissing(t *testing.T) {
	cmd := fakeNextcloudCmd(t)
	cfg := testConfig(t, cmd)
	cfg.Server.Password = ""
	cfg.Server.PasswordFile = "/nonexistent/password"

	executor := NewExecutor(cfg, quietLogger())
	_, err := executor.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for missing password file")
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := testConfig(t, "nextcloudcmd")
	cfg.Sync.ExtraArgs = []string{"--silent"}

	executor := NewExecutor(cfg, quietLogger())
	args := executor.buildArgs()

	expected := []string{
		"--silent",
		"--non-interactive",
		"-n",
		"--path", "/alice",
		"/home/alice/nextcloud",
		"https://cloud.example.com",
	}

	if len(args) != len(expected) {
		t.Fatalf("args len = %d, want %d\ngot:  %v\nwant: %v", len(args), len(expected), args, expected)
	}

	for i, arg := range args {
		if arg != expected[i] {
			t.Errorf("args[%d] = %q, want %q", i, arg, expected[i])
		}
	}

	// Refs #30: no credential — and no username — may appear on argv.
	for _, arg := range args {
		if arg == "-p" || arg == "-u" || arg == "my-password" || arg == "alice" {
			t.Errorf("argv leaks credential/username: found %q in %v", arg, args)
		}
	}
}

func TestBuildArgsNoExtras(t *testing.T) {
	cfg := testConfig(t, "nextcloudcmd")
	cfg.Sync.ExtraArgs = nil

	executor := NewExecutor(cfg, quietLogger())
	args := executor.buildArgs()

	if args[0] != "--non-interactive" {
		t.Errorf("first arg = %q, want --non-interactive", args[0])
	}
}

func TestWriteNetrc(t *testing.T) {
	cfg := testConfig(t, "nextcloudcmd")
	executor := NewExecutor(cfg, quietLogger())

	dir, cleanup, err := executor.writeNetrc("s3cr3t-pw")
	if err != nil {
		t.Fatalf("writeNetrc: %v", err)
	}
	defer cleanup()

	// Directory must be private (0700).
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("netrc dir mode = %o, want 700", perm)
	}

	// .netrc must be 0600 and contain host/login/password.
	path := filepath.Join(dir, ".netrc")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat .netrc: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf(".netrc mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .netrc: %v", err)
	}
	want := "machine cloud.example.com login alice password s3cr3t-pw\n"
	if string(body) != want {
		t.Errorf(".netrc = %q, want %q", string(body), want)
	}

	// Cleanup must remove the directory.
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir still present after cleanup: %v", err)
	}
}

func TestEnvWithHome(t *testing.T) {
	env := envWithHome("/tmp/netrc-home")
	var homeCount int
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			homeCount++
			if kv != "HOME=/tmp/netrc-home" {
				t.Errorf("HOME = %q, want HOME=/tmp/netrc-home", kv)
			}
		}
	}
	if homeCount != 1 {
		t.Errorf("HOME appears %d times, want exactly 1", homeCount)
	}
}

func TestCheckNextcloudCmd(t *testing.T) {
	if err := CheckNextcloudCmd("go"); err != nil {
		t.Errorf("unexpected error for 'go': %v", err)
	}

	err := CheckNextcloudCmd("/nonexistent/binary")
	if err == nil {
		t.Error("expected error for nonexistent binary")
	}

	_ = exec.Command // keep import
}
