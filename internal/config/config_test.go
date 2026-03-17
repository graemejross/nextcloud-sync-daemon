package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidMinimal(t *testing.T) {
	path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret123
sync:
  local_dir: /home/alice/nextcloud
poll:
  enabled: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.URL != "https://cloud.example.com" {
		t.Errorf("server.url = %q, want %q", cfg.Server.URL, "https://cloud.example.com")
	}
	if cfg.Server.Username != "alice" {
		t.Errorf("server.username = %q, want %q", cfg.Server.Username, "alice")
	}
}

func TestLoadValidFull(t *testing.T) {
	path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret123
sync:
  local_dir: /home/alice/nextcloud
  remote_path: /alice
  nextcloudcmd: /usr/bin/nextcloudcmd
  extra_args: ["--non-interactive", "--silent"]
  timeout: 10m
watch:
  enabled: true
  debounce: 15s
  cooldown: 45s
  exclude: ['\.sync_.*\.db', '\.tmp$']
webhook:
  enabled: true
  listen: 0.0.0.0:9000
  secret: my-secret
  path_filter: /alice/
poll:
  enabled: true
  interval: 120s
logging:
  level: debug
  format: json
health:
  enabled: true
  listen: 127.0.0.1:9090
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Sync.RemotePath != "/alice" {
		t.Errorf("sync.remote_path = %q, want %q", cfg.Sync.RemotePath, "/alice")
	}
	if cfg.Sync.Timeout.Duration != 10*time.Minute {
		t.Errorf("sync.timeout = %v, want %v", cfg.Sync.Timeout.Duration, 10*time.Minute)
	}
	if len(cfg.Sync.ExtraArgs) != 2 {
		t.Errorf("sync.extra_args len = %d, want 2", len(cfg.Sync.ExtraArgs))
	}
	if cfg.Watch.Debounce.Duration != 15*time.Second {
		t.Errorf("watch.debounce = %v, want %v", cfg.Watch.Debounce.Duration, 15*time.Second)
	}
	if cfg.Webhook.Listen != "0.0.0.0:9000" {
		t.Errorf("webhook.listen = %q, want %q", cfg.Webhook.Listen, "0.0.0.0:9000")
	}
	if cfg.Poll.Interval.Duration != 120*time.Second {
		t.Errorf("poll.interval = %v, want %v", cfg.Poll.Interval.Duration, 120*time.Second)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("logging.level = %q, want %q", cfg.Logging.Level, "debug")
	}
	if cfg.Health.Listen != "127.0.0.1:9090" {
		t.Errorf("health.listen = %q, want %q", cfg.Health.Listen, "127.0.0.1:9090")
	}
}

func TestDefaults(t *testing.T) {
	path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"nextcloudcmd", cfg.Sync.NextcloudCmd, "nextcloudcmd"},
		{"remote_path", cfg.Sync.RemotePath, "/"},
		{"timeout", cfg.Sync.Timeout.Duration, 5 * time.Minute},
		{"debounce", cfg.Watch.Debounce.Duration, 10 * time.Second},
		{"cooldown", cfg.Watch.Cooldown.Duration, 30 * time.Second},
		{"webhook.listen", cfg.Webhook.Listen, "0.0.0.0:8767"},
		{"poll.interval", cfg.Poll.Interval.Duration, 60 * time.Second},
		{"logging.level", cfg.Logging.Level, "info"},
		{"logging.format", cfg.Logging.Format, "text"},
		{"health.listen", cfg.Health.Listen, "127.0.0.1:8768"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("default %s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr bool
	}{
		{"go string", "timeout: 5m", 5 * time.Minute, false},
		{"go string seconds", "timeout: 30s", 30 * time.Second, false},
		{"integer seconds", "timeout: 120", 120 * time.Second, false},
		{"complex duration", "timeout: 1h30m", 90 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out struct {
				Timeout Duration `yaml:"timeout"`
			}
			err := yamlUnmarshal([]byte(tt.yaml), &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && out.Timeout.Duration != tt.want {
				t.Errorf("duration = %v, want %v", out.Timeout.Duration, tt.want)
			}
		})
	}
}

func yamlUnmarshal(in []byte, out interface{}) error {
	return yaml.Unmarshal(in, out)
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			"missing url",
			`server: {username: a, password: b}
sync: {local_dir: /tmp}
poll: {enabled: true}`,
			"server.url is required",
		},
		{
			"invalid url",
			`server: {url: "not-a-url", username: a, password: b}
sync: {local_dir: /tmp}
poll: {enabled: true}`,
			"not a valid URL",
		},
		{
			"missing username",
			`server: {url: "https://x.com", password: b}
sync: {local_dir: /tmp}
poll: {enabled: true}`,
			"server.username is required",
		},
		{
			"missing password",
			`server: {url: "https://x.com", username: a}
sync: {local_dir: /tmp}
poll: {enabled: true}`,
			"server.password or server.password_file is required",
		},
		{
			"both password and file",
			`server: {url: "https://x.com", username: a, password: b, password_file: /tmp/p}
sync: {local_dir: /tmp}
poll: {enabled: true}`,
			"mutually exclusive",
		},
		{
			"missing local_dir",
			`server: {url: "https://x.com", username: a, password: b}
poll: {enabled: true}`,
			"sync.local_dir is required",
		},
		{
			"webhook without secret",
			`server: {url: "https://x.com", username: a, password: b}
sync: {local_dir: /tmp}
webhook: {enabled: true}`,
			"webhook.secret is required",
		},
		{
			"invalid log level",
			`server: {url: "https://x.com", username: a, password: b}
sync: {local_dir: /tmp}
poll: {enabled: true}
logging: {level: verbose}`,
			"logging.level",
		},
		{
			"invalid log format",
			`server: {url: "https://x.com", username: a, password: b}
sync: {local_dir: /tmp}
poll: {enabled: true}
logging: {format: xml}`,
			"logging.format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.config)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsSubstring(s, substr)
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPasswordFileResolution(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "password")
	if err := os.WriteFile(pwFile, []byte("  file-password\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Server: ServerConfig{PasswordFile: pwFile},
	}

	pw, err := cfg.ResolvePassword()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw != "file-password" {
		t.Errorf("password = %q, want %q", pw, "file-password")
	}
}

func TestPasswordInlineResolution(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Password: "inline-pw"},
	}

	pw, err := cfg.ResolvePassword()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw != "inline-pw" {
		t.Errorf("password = %q, want %q", pw, "inline-pw")
	}
}

func TestPasswordFileMissing(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{PasswordFile: "/nonexistent/password"},
	}

	_, err := cfg.ResolvePassword()
	if err == nil {
		t.Fatal("expected error for missing password file")
	}
}

func TestFindConfigPath(t *testing.T) {
	t.Run("flag takes precedence", func(t *testing.T) {
		path, err := FindConfigPath("/my/config.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if path != "/my/config.yaml" {
			t.Errorf("path = %q, want %q", path, "/my/config.yaml")
		}
	})

	t.Run("env variable", func(t *testing.T) {
		t.Setenv("NEXTCLOUD_SYNC_CONFIG", "/env/config.yaml")
		path, err := FindConfigPath("")
		if err != nil {
			t.Fatal(err)
		}
		if path != "/env/config.yaml" {
			t.Errorf("path = %q, want %q", path, "/env/config.yaml")
		}
	})

	t.Run("user config dir", func(t *testing.T) {
		if _, err := os.Stat("/etc/nextcloud-sync-daemon/config.yaml"); err == nil {
			t.Skip("system config exists, would match before user config dir")
		}
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		t.Setenv("NEXTCLOUD_SYNC_CONFIG", "")

		cfgDir := filepath.Join(dir, ".config", "nextcloud-sync-daemon")
		if err := os.MkdirAll(cfgDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(cfgDir, "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}

		path, err := FindConfigPath("")
		if err != nil {
			t.Fatal(err)
		}
		if path != cfgPath {
			t.Errorf("path = %q, want %q", path, cfgPath)
		}
	})

	t.Run("no config found", func(t *testing.T) {
		if _, err := os.Stat("/etc/nextcloud-sync-daemon/config.yaml"); err == nil {
			t.Skip("system config exists, would always be found")
		}
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		t.Setenv("NEXTCLOUD_SYNC_CONFIG", "")

		_, err := FindConfigPath("")
		if err == nil {
			t.Fatal("expected error when no config found")
		}
	})
}

func TestCheckPasswordFilePermissions(t *testing.T) {
	t.Run("safe permissions", func(t *testing.T) {
		dir := t.TempDir()
		pwFile := filepath.Join(dir, "password")
		if err := os.WriteFile(pwFile, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{Server: ServerConfig{PasswordFile: pwFile}}
		if warn := cfg.CheckPasswordFilePermissions(); warn != "" {
			t.Errorf("expected no warning for 0600, got %q", warn)
		}
	})

	t.Run("world-readable", func(t *testing.T) {
		dir := t.TempDir()
		pwFile := filepath.Join(dir, "password")
		if err := os.WriteFile(pwFile, []byte("secret"), 0644); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{Server: ServerConfig{PasswordFile: pwFile}}
		warn := cfg.CheckPasswordFilePermissions()
		if warn == "" {
			t.Error("expected warning for 0644, got empty string")
		}
		if !containsSubstring(warn, "0644") {
			t.Errorf("warning should mention mode, got %q", warn)
		}
	})

	t.Run("no password file", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{Password: "inline"}}
		if warn := cfg.CheckPasswordFilePermissions(); warn != "" {
			t.Errorf("expected no warning for inline password, got %q", warn)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{PasswordFile: "/nonexistent/password"}}
		if warn := cfg.CheckPasswordFilePermissions(); warn != "" {
			t.Errorf("expected no warning for missing file, got %q", warn)
		}
	})
}

func TestValidateEventSources(t *testing.T) {
	t.Run("no sources", func(t *testing.T) {
		cfg := &Config{}
		if err := cfg.ValidateEventSources(); err == nil {
			t.Fatal("expected error when no sources enabled")
		}
	})

	t.Run("poll enabled", func(t *testing.T) {
		cfg := &Config{Poll: PollConfig{Enabled: true}}
		if err := cfg.ValidateEventSources(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("watch enabled", func(t *testing.T) {
		cfg := &Config{Watch: WatchConfig{Enabled: true}}
		if err := cfg.ValidateEventSources(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("webhook enabled", func(t *testing.T) {
		cfg := &Config{Webhook: WebhookConfig{Enabled: true}}
		if err := cfg.ValidateEventSources(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestLoadWithoutSources verifies that Load succeeds when no event sources
// are enabled — this supports --once mode which doesn't need sources.
func TestLoadWithoutSources(t *testing.T) {
	path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret123
sync:
  local_dir: /home/alice/nextcloud
`)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load should succeed without event sources for --once mode: %v", err)
	}
}

func TestPeerValidation(t *testing.T) {
	t.Run("valid peers", func(t *testing.T) {
		path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
peers:
  - url: "http://other-host:8767/webhook"
    secret: "shared-secret"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Peers) != 1 {
			t.Fatalf("peers len = %d, want 1", len(cfg.Peers))
		}
		if cfg.Peers[0].URL != "http://other-host:8767/webhook" {
			t.Errorf("peer url = %q", cfg.Peers[0].URL)
		}
	})

	t.Run("no peers is valid", func(t *testing.T) {
		path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
`)
		_, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("peer missing url", func(t *testing.T) {
		path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
peers:
  - secret: "s"
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !contains(err.Error(), "peers[0].url is required") {
			t.Errorf("error = %q, want peers[0].url", err.Error())
		}
	})

	t.Run("peer invalid url", func(t *testing.T) {
		path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
peers:
  - url: "not-a-url"
    secret: "s"
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !contains(err.Error(), "not a valid URL") {
			t.Errorf("error = %q, want 'not a valid URL'", err.Error())
		}
	})

	t.Run("peer missing secret", func(t *testing.T) {
		path := writeConfig(t, `
server:
  url: https://cloud.example.com
  username: alice
  password: secret
sync:
  local_dir: /tmp/nc
poll:
  enabled: true
peers:
  - url: "http://host:8767/webhook"
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !contains(err.Error(), "peers[0].secret is required") {
			t.Errorf("error = %q, want peers[0].secret", err.Error())
		}
	})
}

func TestInvalidYAML(t *testing.T) {
	path := writeConfig(t, `{{{invalid yaml`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
