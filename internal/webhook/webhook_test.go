package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
	"github.com/jclement/whaleslap/internal/scheduler"
)

func init() {
	// Initialize logger for tests
	logger.Init(false)
}

type mockServiceProvider struct {
	images map[string]string
}

func (m *mockServiceProvider) GetContainerImage(ctx context.Context, serviceName string) (string, error) {
	if img, ok := m.images[serviceName]; ok {
		return img, nil
	}
	return "unknown:latest", nil
}

func newTestServer() (*Server, *scheduler.Scheduler) {
	cfg := &config.Config{
		WebhookID:  "test-webhook-id",
		Port:       8080,
		Containers: []string{"service1", "service2"},
		// Use weekly window to prevent immediate processing during tests
		// (updates will stay queued since we're usually outside the window)
		UpgradeWindow: config.WindowWeekly,
	}

	checkFunc := func(ctx context.Context, serviceName string) (bool, error) {
		return false, nil
	}
	updateFunc := func(ctx context.Context, serviceName string) error {
		return nil
	}

	sched := scheduler.New(cfg, checkFunc, updateFunc)
	provider := &mockServiceProvider{
		images: map[string]string{
			"service1": "ghcr.io/user/service1:latest",
			"service2": "ghcr.io/user/service2:v1.0",
		},
	}

	server := NewServer(cfg, sched, provider, "1.0.0", "abc123", "2024-01-01")
	return server, sched
}

func TestHandleHealth(t *testing.T) {
	server, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if response["status"] != "healthy" {
		t.Errorf("expected status 'healthy', got %q", response["status"])
	}
}

func TestHandleWebhook_MethodNotAllowed(t *testing.T) {
	server, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/.well-known/whaleslap/test-webhook-id", nil)
	w := httptest.NewRecorder()

	server.handleWebhook(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", w.Code)
	}
}

func TestHandleWebhook_TriggerAll(t *testing.T) {
	server, _ := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/.well-known/whaleslap/test-webhook-id", nil)
	w := httptest.NewRecorder()

	server.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Check response
	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if response["status"] != "queued" {
		t.Errorf("expected status 'queued', got %q", response["status"])
	}

	// Note: We don't check pending updates here because TriggerUpdate
	// spawns a goroutine that may process them before we can check.
	// The important thing is the response indicates queued status.
}

func TestHandleWebhook_TriggerSpecificService(t *testing.T) {
	server, _ := newTestServer()

	body := strings.NewReader(`{"service": "service1"}`)
	req := httptest.NewRequest(http.MethodPost, "/.well-known/whaleslap/test-webhook-id", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Check response indicates success
	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if response["status"] != "queued" {
		t.Errorf("expected status 'queued', got %q", response["status"])
	}
}

func TestHandleWebhook_ServiceNotFound(t *testing.T) {
	server, _ := newTestServer()

	body := strings.NewReader(`{"service": "nonexistent"}`)
	req := httptest.NewRequest(http.MethodPost, "/.well-known/whaleslap/test-webhook-id", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleWebhook(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestHandleStatusJSON(t *testing.T) {
	server, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	server.handleStatusJSON(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}

	var response StatusResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if response.Version != "1.0.0" {
		t.Errorf("expected version '1.0.0', got %q", response.Version)
	}
	if response.Commit != "abc123" {
		t.Errorf("expected commit 'abc123', got %q", response.Commit)
	}
	if !response.Healthy {
		t.Error("expected healthy to be true")
	}
	if len(response.Services) != 2 {
		t.Errorf("expected 2 services, got %d", len(response.Services))
	}
}

func TestHandleStatusPage(t *testing.T) {
	server, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/.well-known/whaleslap/test-webhook-id/status", nil)
	w := httptest.NewRecorder()

	server.handleStatusPage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("expected Content-Type 'text/html', got %q", contentType)
	}

	body := w.Body.String()
	if !strings.Contains(body, "WhaleSlap") {
		t.Error("expected 'WhaleSlap' in HTML body")
	}
	if !strings.Contains(body, "service1") {
		t.Error("expected 'service1' in HTML body")
	}
}

func TestGetWebhookURL(t *testing.T) {
	server, _ := newTestServer()

	url := server.GetWebhookURL()
	expected := "/.well-known/whaleslap/test-webhook-id"
	if url != expected {
		t.Errorf("expected %q, got %q", expected, url)
	}
}

func TestFormatCurlExample(t *testing.T) {
	result := FormatCurlExample("http://localhost:8080", "my-webhook-id")
	expected := "curl -X POST http://localhost:8080/.well-known/whaleslap/my-webhook-id"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}

	// Test with trailing slash
	result = FormatCurlExample("http://localhost:8080/", "my-webhook-id")
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"minutes only", 5 * time.Minute, "5m"},
		{"hours and minutes", 2*time.Hour + 30*time.Minute, "2h 30m"},
		{"days hours minutes", 49*time.Hour + 30*time.Minute, "2d 1h 30m"},
		{"zero", 0, "0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.duration, result, tt.expected)
			}
		})
	}
}

func TestGenerateClientID(t *testing.T) {
	id1 := generateClientID()
	id2 := generateClientID()

	// Should be 16 hex chars (8 bytes * 2)
	if len(id1) != 16 {
		t.Errorf("expected 16 char ID, got %d", len(id1))
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

func TestGetStatus_WithPendingUpdates(t *testing.T) {
	server, sched := newTestServer()

	// Queue an update
	sched.QueueUpdate("service1")

	status := server.getStatus()

	if len(status.PendingUpdates) != 1 {
		t.Errorf("expected 1 pending update, got %d", len(status.PendingUpdates))
	}

	// Check service status shows update queued
	for _, svc := range status.Services {
		if svc.Name == "service1" {
			if !svc.UpdateQueued {
				t.Error("expected service1 to have UpdateQueued=true")
			}
		}
	}
}
