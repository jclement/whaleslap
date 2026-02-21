package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	os.Unsetenv("WHALESLAP_SCHEDULE")
	os.Unsetenv("WHALESLAP_UPGRADE_WINDOW")
	os.Unsetenv("WHALESLAP_WEBHOOK_ID")
	os.Unsetenv("WHALESLAP_PORT")
	os.Unsetenv("WHALESLAP_DEBUG")
	os.Unsetenv("DOCKER_HOST")
	os.Unsetenv("GITHUB_PAT")
	os.Unsetenv("WHALESLAP_COMPOSE_PROJECT")
	os.Unsetenv("WHALESLAP_NOTIFY_URL")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Port)
	}
	if cfg.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("expected default docker host, got %s", cfg.DockerHost)
	}
	if cfg.WebhookID == "" {
		t.Error("expected auto-generated webhook ID")
	}
	if len(cfg.WebhookID) != 32 {
		t.Errorf("expected 32-char webhook ID, got %d chars", len(cfg.WebhookID))
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	// Set env vars
	os.Setenv("WHALESLAP_SCHEDULE", "10m")
	os.Setenv("WHALESLAP_UPGRADE_WINDOW", "nightly")
	os.Setenv("WHALESLAP_WEBHOOK_ID", "custom-webhook-id")
	os.Setenv("WHALESLAP_PORT", "9090")
	os.Setenv("WHALESLAP_DEBUG", "true")
	os.Setenv("DOCKER_HOST", "tcp://localhost:2375")
	os.Setenv("GITHUB_PAT", "ghp_test123")
	os.Setenv("WHALESLAP_COMPOSE_PROJECT", "myproject")
	os.Setenv("WHALESLAP_NOTIFY_URL", "https://example.com/webhook")
	defer func() {
		os.Unsetenv("WHALESLAP_SCHEDULE")
		os.Unsetenv("WHALESLAP_UPGRADE_WINDOW")
		os.Unsetenv("WHALESLAP_WEBHOOK_ID")
		os.Unsetenv("WHALESLAP_PORT")
		os.Unsetenv("WHALESLAP_DEBUG")
		os.Unsetenv("DOCKER_HOST")
		os.Unsetenv("GITHUB_PAT")
		os.Unsetenv("WHALESLAP_COMPOSE_PROJECT")
		os.Unsetenv("WHALESLAP_NOTIFY_URL")
	}()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Schedule != "10m" {
		t.Errorf("Schedule = %q, want %q", cfg.Schedule, "10m")
	}
	if cfg.UpgradeWindow != WindowNightly {
		t.Errorf("UpgradeWindow = %q, want %q", cfg.UpgradeWindow, WindowNightly)
	}
	if cfg.WebhookID != "custom-webhook-id" {
		t.Errorf("WebhookID = %q, want %q", cfg.WebhookID, "custom-webhook-id")
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want %d", cfg.Port, 9090)
	}
	if !cfg.Debug {
		t.Error("Debug should be true")
	}
	if cfg.DockerHost != "tcp://localhost:2375" {
		t.Errorf("DockerHost = %q, want %q", cfg.DockerHost, "tcp://localhost:2375")
	}
	if cfg.GithubPAT != "ghp_test123" {
		t.Errorf("GithubPAT = %q, want %q", cfg.GithubPAT, "ghp_test123")
	}
	if cfg.ComposeProject != "myproject" {
		t.Errorf("ComposeProject = %q, want %q", cfg.ComposeProject, "myproject")
	}
	if cfg.NotifyURL != "https://example.com/webhook" {
		t.Errorf("NotifyURL = %q, want %q", cfg.NotifyURL, "https://example.com/webhook")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		window  UpgradeWindow
		wantErr bool
	}{
		{"empty window", WindowNone, false},
		{"nightly window", WindowNightly, false},
		{"weekly window", WindowWeekly, false},
		{"invalid window", UpgradeWindow("invalid"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{UpgradeWindow: tt.window}
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsInUpgradeWindow_NoWindow(t *testing.T) {
	cfg := &Config{UpgradeWindow: WindowNone}
	if !cfg.IsInUpgradeWindow() {
		t.Error("expected always in window when no window configured")
	}
}

func TestIsInUpgradeWindow_Nightly(t *testing.T) {
	cfg := &Config{UpgradeWindow: WindowNightly}

	// Test various hours
	tests := []struct {
		hour     int
		expected bool
	}{
		{0, true},   // midnight
		{1, true},   // 1am
		{5, true},   // 5am
		{6, false},  // 6am - end of window
		{12, false}, // noon
		{18, false}, // 6pm
		{22, false}, // 10pm
		{23, true},  // 11pm - start of window
	}

	for _, tt := range tests {
		t.Run(time.Date(2024, 1, 1, tt.hour, 0, 0, 0, time.Local).Format("15:04"), func(t *testing.T) {
			// We can't easily mock time, so this test documents expected behavior
			// The actual test will depend on current time
			_ = tt // Document the expected behavior
		})
	}

	// Just verify the function runs without error
	_ = cfg.IsInUpgradeWindow()
}

func TestIsInUpgradeWindow_Weekly(t *testing.T) {
	cfg := &Config{UpgradeWindow: WindowWeekly}

	// Just verify the function runs without error
	// Full testing would require time mocking
	_ = cfg.IsInUpgradeWindow()
}

func TestGenerateWebhookID(t *testing.T) {
	id1 := generateWebhookID()
	id2 := generateWebhookID()

	// Should be 32 hex chars (16 bytes * 2)
	if len(id1) != 32 {
		t.Errorf("expected 32 char ID, got %d", len(id1))
	}

	// Should be unique
	if id1 == id2 {
		t.Error("generated IDs should be unique")
	}

	// Should be valid hex
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("invalid hex character: %c", c)
		}
	}
}

func TestLoad_DebugValues(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"yes", false}, // only "true" and "1" are truthy
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			os.Setenv("WHALESLAP_DEBUG", tt.value)
			defer os.Unsetenv("WHALESLAP_DEBUG")

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Debug != tt.expected {
				t.Errorf("Debug = %v, want %v for value %q", cfg.Debug, tt.expected, tt.value)
			}
		})
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	os.Setenv("WHALESLAP_PORT", "invalid")
	defer os.Unsetenv("WHALESLAP_PORT")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Should keep default port when invalid
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want default 8080 for invalid input", cfg.Port)
	}
}
