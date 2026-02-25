package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type UpgradeWindow string

const (
	WindowNone    UpgradeWindow = ""
	WindowNightly UpgradeWindow = "nightly"
	WindowWeekly  UpgradeWindow = "weekly"
)

type Config struct {
	// Schedule for checking updates (cron expression or duration like "5m", "1h")
	Schedule string `yaml:"schedule"`

	// UpgradeWindow restricts when updates can be applied
	// "nightly" = 11pm-6am, "weekly" = Sunday 6pm-Monday 6am
	UpgradeWindow UpgradeWindow `yaml:"upgrade_window"`

	// GitHub PAT for pulling private images from GHCR
	GithubPAT string `yaml:"github_pat"`

	// Containers holds discovered service names (populated at runtime)
	Containers []string

	// WebhookID is the unique identifier for the webhook endpoint
	// Auto-generated if not specified
	WebhookID string `yaml:"webhook_id"`

	// Port for the web server
	Port int `yaml:"port"`

	// Debug enables verbose logging
	Debug bool `yaml:"debug"`

	// DockerHost is the Docker socket path (default: unix:///var/run/docker.sock)
	DockerHost string `yaml:"docker_host"`

	// NotifyURL is an optional webhook URL to POST status updates to
	// Receives JSON payloads for events like update_started, update_completed, update_failed
	NotifyURL string `yaml:"notify_url"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Port:       8080,
		DockerHost: "unix:///var/run/docker.sock",
	}

	// Load from file if exists
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	// Environment variables override config file
	cfg.loadEnv()

	// Generate webhook ID if not set
	if cfg.WebhookID == "" {
		cfg.WebhookID = generateWebhookID()
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) loadEnv() {
	if v := os.Getenv("WHALESLAP_SCHEDULE"); v != "" {
		c.Schedule = v
	}
	if v := os.Getenv("WHALESLAP_UPGRADE_WINDOW"); v != "" {
		c.UpgradeWindow = UpgradeWindow(v)
	}
	if v := os.Getenv("GITHUB_PAT"); v != "" {
		c.GithubPAT = v
	}
	if v := os.Getenv("WHALESLAP_WEBHOOK_ID"); v != "" {
		c.WebhookID = v
	}
	if v := os.Getenv("WHALESLAP_PORT"); v != "" {
		var port int
		fmt.Sscanf(v, "%d", &port)
		if port > 0 {
			c.Port = port
		}
	}
	if v := os.Getenv("WHALESLAP_DEBUG"); v != "" {
		c.Debug = v == "true" || v == "1"
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		c.DockerHost = v
	}
	if v := os.Getenv("WHALESLAP_NOTIFY_URL"); v != "" {
		c.NotifyURL = v
	}
}

func (c *Config) Validate() error {
	if c.UpgradeWindow != WindowNone &&
		c.UpgradeWindow != WindowNightly &&
		c.UpgradeWindow != WindowWeekly {
		return fmt.Errorf("invalid upgrade_window: %q (must be 'nightly' or 'weekly')", c.UpgradeWindow)
	}

	return nil
}

// IsInUpgradeWindow checks if the current time is within the upgrade window
func (c *Config) IsInUpgradeWindow() bool {
	if c.UpgradeWindow == WindowNone {
		return true
	}

	now := time.Now()
	hour := now.Hour()
	weekday := now.Weekday()

	switch c.UpgradeWindow {
	case WindowNightly:
		// 11pm - 6am
		return hour >= 23 || hour < 6
	case WindowWeekly:
		// Sunday 6pm - Monday 6am
		if weekday == time.Sunday && hour >= 18 {
			return true
		}
		if weekday == time.Monday && hour < 6 {
			return true
		}
		return false
	}

	return true
}

func generateWebhookID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		// Fall back to time-based ID if crypto/rand fails
		// This is extremely unlikely but we should handle it
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}
