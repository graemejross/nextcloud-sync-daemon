// Package config handles YAML configuration loading, validation, and defaults.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration for YAML unmarshaling.
// Accepts Go duration strings ("5m", "30s") or integer seconds.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	// Try integer seconds first (YAML integers won't parse as Go durations)
	var secs int
	if err := value.Decode(&secs); err == nil && value.Tag == "!!int" {
		d.Duration = time.Duration(secs) * time.Second
		return nil
	}

	// Try Go duration string ("5m", "30s", "1h30m")
	var s string
	if err := value.Decode(&s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = parsed
		return nil
	}

	return fmt.Errorf("duration must be a string (\"5m\") or integer seconds")
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// Config is the top-level configuration.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Sync    SyncConfig    `yaml:"sync"`
	Watch   WatchConfig   `yaml:"watch"`
	Webhook WebhookConfig `yaml:"webhook"`
	Poll    PollConfig    `yaml:"poll"`
	Logging LogConfig     `yaml:"logging"`
	Health  HealthConfig  `yaml:"health"`
}

type ServerConfig struct {
	URL          string `yaml:"url"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

type SyncConfig struct {
	LocalDir     string   `yaml:"local_dir"`
	RemotePath   string   `yaml:"remote_path"`
	NextcloudCmd string   `yaml:"nextcloudcmd"`
	ExtraArgs    []string `yaml:"extra_args"`
	Timeout      Duration `yaml:"timeout"`
}

type WatchConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Debounce Duration `yaml:"debounce"`
	Cooldown Duration `yaml:"cooldown"`
	Exclude  []string `yaml:"exclude"`
}

type WebhookConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Listen     string `yaml:"listen"`
	Secret     string `yaml:"secret"`
	PathFilter string `yaml:"path_filter"`
}

type PollConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type HealthConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// Load reads and parses a config file, applies defaults, and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Sync.NextcloudCmd == "" {
		cfg.Sync.NextcloudCmd = "nextcloudcmd"
	}
	if cfg.Sync.RemotePath == "" {
		cfg.Sync.RemotePath = "/"
	}
	if cfg.Sync.Timeout.Duration == 0 {
		cfg.Sync.Timeout.Duration = 5 * time.Minute
	}

	if cfg.Watch.Debounce.Duration == 0 {
		cfg.Watch.Debounce.Duration = 10 * time.Second
	}
	if cfg.Watch.Cooldown.Duration == 0 {
		cfg.Watch.Cooldown.Duration = 30 * time.Second
	}
	if cfg.Watch.Exclude == nil {
		cfg.Watch.Exclude = []string{`\.sync_.*\.db`}
	}

	if cfg.Webhook.Listen == "" {
		cfg.Webhook.Listen = "0.0.0.0:8767"
	}
	if cfg.Webhook.PathFilter == "" {
		cfg.Webhook.PathFilter = "/"
	}

	if cfg.Poll.Interval.Duration == 0 {
		cfg.Poll.Interval.Duration = 60 * time.Second
	}

	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "text"
	}

	if cfg.Health.Listen == "" {
		cfg.Health.Listen = "127.0.0.1:8768"
	}

	// Default: watch and poll enabled, webhook and health disabled
	// These are only applied if the YAML didn't set them — but since Go zero
	// values for bool are false, we need a different approach for "default true".
	// We handle this by documenting that watch/poll default to enabled only when
	// not explicitly set. Since YAML bool false == Go zero value, we can't
	// distinguish "not set" from "explicitly false" without pointer types.
	// For simplicity, we don't default booleans — users must enable sources.
}

// Validate checks the config for errors. Returns all problems at once.
func (c *Config) Validate() error {
	var errs []error

	// Server
	if c.Server.URL == "" {
		errs = append(errs, errors.New("server.url is required"))
	} else if u, err := url.Parse(c.Server.URL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("server.url %q is not a valid URL", c.Server.URL))
	}

	if c.Server.Username == "" {
		errs = append(errs, errors.New("server.username is required"))
	}

	if c.Server.Password == "" && c.Server.PasswordFile == "" {
		errs = append(errs, errors.New("server.password or server.password_file is required"))
	}
	if c.Server.Password != "" && c.Server.PasswordFile != "" {
		errs = append(errs, errors.New("server.password and server.password_file are mutually exclusive"))
	}

	// Sync
	if c.Sync.LocalDir == "" {
		errs = append(errs, errors.New("sync.local_dir is required"))
	}

	if c.Sync.Timeout.Duration <= 0 {
		errs = append(errs, errors.New("sync.timeout must be positive"))
	}

	// Watch
	if c.Watch.Debounce.Duration <= 0 {
		errs = append(errs, errors.New("watch.debounce must be positive"))
	}
	if c.Watch.Cooldown.Duration < c.Watch.Debounce.Duration {
		errs = append(errs, errors.New("watch.cooldown must be >= watch.debounce"))
	}

	// Webhook
	if c.Webhook.Enabled && c.Webhook.Secret == "" {
		errs = append(errs, errors.New("webhook.secret is required when webhook is enabled"))
	}

	// Poll
	if c.Poll.Interval.Duration <= 0 {
		errs = append(errs, errors.New("poll.interval must be positive"))
	}

	// Logging
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		errs = append(errs, fmt.Errorf("logging.level %q is invalid (use debug, info, warn, error)", c.Logging.Level))
	}

	validFormats := map[string]bool{"text": true, "json": true}
	if !validFormats[c.Logging.Format] {
		errs = append(errs, fmt.Errorf("logging.format %q is invalid (use text, json)", c.Logging.Format))
	}

	// At least one event source
	if !c.Watch.Enabled && !c.Webhook.Enabled && !c.Poll.Enabled {
		errs = append(errs, errors.New("at least one event source (watch, webhook, poll) must be enabled"))
	}

	return errors.Join(errs...)
}

// ResolvePassword returns the password, reading from password_file if configured.
// Called at sync time (not cached) to support credential rotation.
func (c *Config) ResolvePassword() (string, error) {
	if c.Server.Password != "" {
		return c.Server.Password, nil
	}

	data, err := os.ReadFile(c.Server.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading password file: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// FindConfigPath resolves the config file path using the 4-level resolution order:
// 1. flagValue (--config flag)
// 2. NEXTCLOUD_SYNC_CONFIG environment variable
// 3. /etc/nextcloud-sync-daemon/config.yaml
// 4. ~/.config/nextcloud-sync-daemon/config.yaml
func FindConfigPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	if envPath := os.Getenv("NEXTCLOUD_SYNC_CONFIG"); envPath != "" {
		return envPath, nil
	}

	systemPath := "/etc/nextcloud-sync-daemon/config.yaml"
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath, nil
	}

	home, err := os.UserHomeDir()
	if err == nil {
		userPath := filepath.Join(home, ".config", "nextcloud-sync-daemon", "config.yaml")
		if _, err := os.Stat(userPath); err == nil {
			return userPath, nil
		}
	}

	return "", errors.New("no config file found (use --config, $NEXTCLOUD_SYNC_CONFIG, /etc/nextcloud-sync-daemon/config.yaml, or ~/.config/nextcloud-sync-daemon/config.yaml)")
}
