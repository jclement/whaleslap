package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
	"github.com/jclement/whaleslap/internal/scheduler"
)

type Server struct {
	cfg       *config.Config
	scheduler *scheduler.Scheduler
	server    *http.Server
	clients   map[string]chan []byte
	clientsMu sync.RWMutex
}

func NewServer(cfg *config.Config, sched *scheduler.Scheduler) *Server {
	return &Server{
		cfg:       cfg,
		scheduler: sched,
		clients:   make(map[string]chan []byte),
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Webhook endpoint - durable URL for GitHub Actions
	webhookPath := fmt.Sprintf("/.well-known/whaleslap/%s", s.cfg.WebhookID)
	mux.HandleFunc(webhookPath, s.handleWebhook)

	// Server-Sent Events for real-time updates
	mux.HandleFunc(webhookPath+"/events", s.handleSSE)

	// Health check
	mux.HandleFunc("/health", s.handleHealth)

	// Status endpoint
	mux.HandleFunc("/status", s.handleStatus)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.cfg.Port),
		Handler:      s.logMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	logger.Info("starting webhook server",
		"port", s.cfg.Port,
		"webhook_url", fmt.Sprintf("http://localhost:%d%s", s.cfg.Port, webhookPath))

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("webhook server error", "error", err)
		}
	}()

	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(start))
	})
}

// WebhookPayload represents the expected payload
// Compatible with GitHub repository webhooks - any POST triggers all services
type WebhookPayload struct {
	// Service name to update (compose service name) - optional
	Service string `json:"service,omitempty"`
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Try to parse payload, but don't require it
	// This makes it compatible with GitHub webhooks which send their own payload format
	var payload WebhookPayload
	_ = json.NewDecoder(r.Body).Decode(&payload)

	// If a specific service is requested and configured, update just that one
	if payload.Service != "" {
		if !s.scheduler.IsConfigured(payload.Service) {
			logger.Warn("service not found in configuration", "service", payload.Service)
			http.Error(w, "service not configured", http.StatusNotFound)
			return
		}
		logger.Info("webhook received", "service", payload.Service)
		s.scheduler.TriggerUpdate(payload.Service)
		s.broadcast([]byte(fmt.Sprintf(`{"event":"update_queued","target":"%s"}`, payload.Service)))
	} else {
		// Default: trigger update for ALL configured services
		// This makes it drop-in compatible with GitHub repository webhooks
		logger.Info("webhook received", "target", "all")
		for _, serviceName := range s.cfg.Containers {
			s.scheduler.TriggerUpdate(serviceName)
		}
		s.broadcast([]byte(`{"event":"update_queued","target":"all"}`))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "queued",
		"message": "update has been queued",
	})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientID := r.RemoteAddr
	events := make(chan []byte, 10)

	s.clientsMu.Lock()
	s.clients[clientID] = events
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, clientID)
		s.clientsMu.Unlock()
		close(events)
	}()

	logger.Debug("SSE client connected", "client", clientID)

	// Send initial status
	status := s.getStatus()
	statusJSON, _ := json.Marshal(status)
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", statusJSON)
	w.(http.Flusher).Flush()

	for {
		select {
		case <-r.Context().Done():
			logger.Debug("SSE client disconnected", "client", clientID)
			return
		case msg := <-events:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		}
	}
}

func (s *Server) broadcast(msg []byte) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for _, ch := range s.clients {
		select {
		case ch <- msg:
		default:
			// Client buffer full, skip
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

type StatusResponse struct {
	Healthy        bool                       `json:"healthy"`
	WebhookURL     string                     `json:"webhook_url"`
	UpgradeWindow  string                     `json:"upgrade_window,omitempty"`
	InWindow       bool                       `json:"in_window"`
	Schedule       string                     `json:"schedule,omitempty"`
	Services       []string                   `json:"services"`
	PendingUpdates []*scheduler.PendingUpdate `json:"pending_updates"`
}

func (s *Server) getStatus() StatusResponse {
	webhookURL := fmt.Sprintf("/.well-known/whaleslap/%s", s.cfg.WebhookID)

	return StatusResponse{
		Healthy:        true,
		WebhookURL:     webhookURL,
		UpgradeWindow:  string(s.cfg.UpgradeWindow),
		InWindow:       s.cfg.IsInUpgradeWindow(),
		Schedule:       s.cfg.Schedule,
		Services:       s.cfg.Containers,
		PendingUpdates: s.scheduler.GetPendingUpdates(),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.getStatus())
}

// GetWebhookURL returns the full webhook URL path
func (s *Server) GetWebhookURL() string {
	return fmt.Sprintf("/.well-known/whaleslap/%s", s.cfg.WebhookID)
}

// FormatCurlExample returns a curl command example for triggering updates
func FormatCurlExample(baseURL, webhookID, serviceName string) string {
	webhookURL := fmt.Sprintf("%s/.well-known/whaleslap/%s", strings.TrimSuffix(baseURL, "/"), webhookID)
	return fmt.Sprintf(`curl -X POST %s \
  -H "Content-Type: application/json" \
  -d '{"service": "%s"}'`, webhookURL, serviceName)
}
