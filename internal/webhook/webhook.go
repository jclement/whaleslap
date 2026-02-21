package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
	"github.com/jclement/whaleslap/internal/scheduler"
)

// ServiceInfoProvider provides information about monitored services
type ServiceInfoProvider interface {
	GetContainerImage(ctx context.Context, serviceName string) (string, error)
}

type Server struct {
	cfg             *config.Config
	scheduler       *scheduler.Scheduler
	serviceProvider ServiceInfoProvider
	server          *http.Server
	clients         map[string]chan []byte
	clientsMu       sync.RWMutex
	version         string
	commit          string
	buildDate       string
	startTime       time.Time
}

func NewServer(cfg *config.Config, sched *scheduler.Scheduler, provider ServiceInfoProvider, version, commit, buildDate string) *Server {
	return &Server{
		cfg:             cfg,
		scheduler:       sched,
		serviceProvider: provider,
		clients:         make(map[string]chan []byte),
		version:         version,
		commit:          commit,
		buildDate:       buildDate,
		startTime:       time.Now(),
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

	// Status endpoints under webhook path
	mux.HandleFunc(webhookPath+"/status", s.handleStatusPage)
	mux.HandleFunc(webhookPath+"/status/data", s.handleStatusJSON)

	// Legacy status endpoint
	mux.HandleFunc("/status", s.handleStatusJSON)

	s.server = &http.Server{
		Addr:         fmt.Sprintf("0.0.0.0:%d", s.cfg.Port),
		Handler:      s.logMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	logger.Info("starting webhook server",
		"port", s.cfg.Port,
		"addr", s.server.Addr,
		"webhook_url", fmt.Sprintf("http://localhost:%d%s", s.cfg.Port, webhookPath))

	// Start server and check for immediate bind errors
	errCh := make(chan error, 1)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("webhook server error", "error", err)
			errCh <- err
		}
	}()

	// Give the server a moment to start and check for errors
	select {
	case err := <-errCh:
		return fmt.Errorf("failed to start webhook server: %w", err)
	case <-time.After(100 * time.Millisecond):
		// Server started successfully
	}

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

	// Generate unique client ID to avoid collisions from same IP
	clientID := generateClientID()
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

type ServiceStatus struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	HasUpdate    bool   `json:"has_update"`
	UpdateQueued bool   `json:"update_queued"`
}

type StatusResponse struct {
	Version        string                     `json:"version"`
	Commit         string                     `json:"commit"`
	BuildDate      string                     `json:"build_date"`
	Uptime         string                     `json:"uptime"`
	UptimeSeconds  int64                      `json:"uptime_seconds"`
	Healthy        bool                       `json:"healthy"`
	WebhookURL     string                     `json:"webhook_url"`
	UpgradeWindow  string                     `json:"upgrade_window,omitempty"`
	InWindow       bool                       `json:"in_window"`
	Schedule       string                     `json:"schedule,omitempty"`
	NextCheck      string                     `json:"next_check,omitempty"`
	Services       []ServiceStatus            `json:"services"`
	PendingUpdates []*scheduler.PendingUpdate `json:"pending_updates"`
}

func (s *Server) getStatus() StatusResponse {
	webhookURL := fmt.Sprintf("/.well-known/whaleslap/%s", s.cfg.WebhookID)
	uptime := time.Since(s.startTime)

	// Get pending updates as a map for quick lookup
	pendingMap := make(map[string]bool)
	for _, p := range s.scheduler.GetPendingUpdates() {
		pendingMap[p.ServiceName] = true
	}

	// Build service status list
	var services []ServiceStatus
	ctx := context.Background()
	for _, name := range s.cfg.Containers {
		image := "unknown"
		if s.serviceProvider != nil {
			if img, err := s.serviceProvider.GetContainerImage(ctx, name); err == nil {
				image = img
			}
		}
		services = append(services, ServiceStatus{
			Name:         name,
			Image:        image,
			HasUpdate:    false, // Would need to check registry
			UpdateQueued: pendingMap[name],
		})
	}

	// Calculate next check time
	nextCheck := ""
	if s.cfg.Schedule != "" {
		nextCheck = s.scheduler.GetNextCheckTime()
	}

	return StatusResponse{
		Version:        s.version,
		Commit:         s.commit,
		BuildDate:      s.buildDate,
		Uptime:         formatDuration(uptime),
		UptimeSeconds:  int64(uptime.Seconds()),
		Healthy:        true,
		WebhookURL:     webhookURL,
		UpgradeWindow:  string(s.cfg.UpgradeWindow),
		InWindow:       s.cfg.IsInUpgradeWindow(),
		Schedule:       s.cfg.Schedule,
		NextCheck:      nextCheck,
		Services:       services,
		PendingUpdates: s.scheduler.GetPendingUpdates(),
	}
}

func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.getStatus())
}

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	status := s.getStatus()

	tmpl := template.Must(template.New("status").Parse(statusPageTemplate))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, status)
}

const statusPageTemplate = `<!DOCTYPE html>
<html>
<head>
    <title>WhaleSlap Status</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            background: #0f172a; color: #e2e8f0; padding: 2rem;
            min-height: 100vh;
        }
        .container { max-width: 800px; margin: 0 auto; }
        h1 { font-size: 2rem; margin-bottom: 0.5rem; display: flex; align-items: center; gap: 0.5rem; }
        h1::before { content: "🐋"; }
        .version { color: #64748b; font-size: 0.875rem; margin-bottom: 2rem; }
        .card {
            background: #1e293b; border-radius: 0.75rem; padding: 1.5rem;
            margin-bottom: 1rem; border: 1px solid #334155;
        }
        .card h2 { font-size: 0.875rem; text-transform: uppercase; color: #64748b; margin-bottom: 1rem; letter-spacing: 0.05em; }
        .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 1rem; }
        .stat { }
        .stat-value { font-size: 1.5rem; font-weight: 600; color: #f8fafc; }
        .stat-label { font-size: 0.75rem; color: #64748b; text-transform: uppercase; }
        .service {
            display: flex; justify-content: space-between; align-items: center;
            padding: 0.75rem 0; border-bottom: 1px solid #334155;
        }
        .service:last-child { border-bottom: none; }
        .service-name { font-weight: 500; }
        .service-image { font-size: 0.875rem; color: #64748b; font-family: monospace; }
        .badge {
            font-size: 0.75rem; padding: 0.25rem 0.5rem; border-radius: 9999px;
            font-weight: 500;
        }
        .badge-green { background: #065f46; color: #6ee7b7; }
        .badge-yellow { background: #78350f; color: #fcd34d; }
        .badge-gray { background: #374151; color: #9ca3af; }
        .window-status { display: flex; align-items: center; gap: 0.5rem; }
        .dot { width: 8px; height: 8px; border-radius: 50%; }
        .dot-green { background: #22c55e; }
        .dot-red { background: #ef4444; }
        .webhook-url {
            font-family: monospace; font-size: 0.875rem; background: #0f172a;
            padding: 0.75rem; border-radius: 0.5rem; word-break: break-all;
        }
        .refresh { color: #64748b; font-size: 0.75rem; text-align: center; margin-top: 2rem; }
    </style>
</head>
<body>
    <div class="container">
        <h1>WhaleSlap</h1>
        <div class="version">{{.Version}} ({{.Commit}}) • Up {{.Uptime}}</div>

        <div class="card">
            <h2>Status</h2>
            <div class="grid">
                <div class="stat">
                    <div class="stat-value">{{len .Services}}</div>
                    <div class="stat-label">Services</div>
                </div>
                <div class="stat">
                    <div class="stat-value">{{len .PendingUpdates}}</div>
                    <div class="stat-label">Pending Updates</div>
                </div>
                <div class="stat">
                    <div class="stat-value">{{if .Schedule}}{{.Schedule}}{{else}}—{{end}}</div>
                    <div class="stat-label">Check Interval</div>
                </div>
                <div class="stat">
                    <div class="window-status">
                        {{if .UpgradeWindow}}
                            <span class="dot {{if .InWindow}}dot-green{{else}}dot-red{{end}}"></span>
                            <span>{{.UpgradeWindow}}</span>
                        {{else}}
                            <span class="badge badge-green">Always</span>
                        {{end}}
                    </div>
                    <div class="stat-label">Upgrade Window</div>
                </div>
            </div>
        </div>

        <div class="card">
            <h2>Monitored Services</h2>
            {{range .Services}}
            <div class="service">
                <div>
                    <div class="service-name">{{.Name}}</div>
                    <div class="service-image">{{.Image}}</div>
                </div>
                {{if .UpdateQueued}}
                    <span class="badge badge-yellow">Update Queued</span>
                {{else}}
                    <span class="badge badge-green">Current</span>
                {{end}}
            </div>
            {{end}}
        </div>

        <div class="refresh">Auto-refreshes every 30 seconds</div>
    </div>
    <script>setTimeout(() => location.reload(), 30000);</script>
</body>
</html>`

// GetWebhookURL returns the full webhook URL path
func (s *Server) GetWebhookURL() string {
	return fmt.Sprintf("/.well-known/whaleslap/%s", s.cfg.WebhookID)
}

// generateClientID creates a unique ID for SSE clients
func generateClientID() string {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to timestamp if crypto/rand fails
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

// FormatCurlExample returns a curl command example for triggering updates
func FormatCurlExample(baseURL, webhookID string) string {
	webhookURL := fmt.Sprintf("%s/.well-known/whaleslap/%s", strings.TrimSuffix(baseURL, "/"), webhookID)
	return fmt.Sprintf("curl -X POST %s", webhookURL)
}
