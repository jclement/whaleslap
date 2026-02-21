package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
)

// EventType represents the type of notification event
type EventType string

const (
	EventUpdateQueued    EventType = "update_queued"
	EventUpdateStarted   EventType = "update_started"
	EventUpdateCompleted EventType = "update_completed"
	EventUpdateFailed    EventType = "update_failed"
	EventCheckStarted    EventType = "check_started"
	EventCheckCompleted  EventType = "check_completed"
)

// Notification represents a status update notification
type Notification struct {
	Event     EventType `json:"event"`
	Service   string    `json:"service,omitempty"`
	Message   string    `json:"message,omitempty"`
	Error     string    `json:"error,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Notifier sends status updates to configured webhooks
type Notifier struct {
	cfg    *config.Config
	client *http.Client
}

// New creates a new Notifier
func New(cfg *config.Config) *Notifier {
	return &Notifier{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Notify sends a notification to the configured webhook URL
func (n *Notifier) Notify(ctx context.Context, event EventType, service, message string) {
	if n.cfg.NotifyURL == "" {
		return
	}

	notification := Notification{
		Event:     event,
		Service:   service,
		Message:   message,
		Timestamp: time.Now(),
	}

	n.send(ctx, notification)
}

// NotifyError sends an error notification
func (n *Notifier) NotifyError(ctx context.Context, event EventType, service string, err error) {
	if n.cfg.NotifyURL == "" {
		return
	}

	notification := Notification{
		Event:     event,
		Service:   service,
		Error:     err.Error(),
		Timestamp: time.Now(),
	}

	n.send(ctx, notification)
}

func (n *Notifier) send(ctx context.Context, notification Notification) {
	body, err := json.Marshal(notification)
	if err != nil {
		logger.Error("failed to marshal notification", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.NotifyURL, bytes.NewReader(body))
	if err != nil {
		logger.Error("failed to create notification request", "error", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WhaleSlap/1.0")

	// Send async to avoid blocking updates
	go func() {
		resp, err := n.client.Do(req)
		if err != nil {
			logger.Warn("failed to send notification", "url", n.cfg.NotifyURL, "error", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			logger.Warn("notification webhook returned error", "url", n.cfg.NotifyURL, "status", resp.StatusCode)
			return
		}

		logger.Debug("notification sent", "event", notification.Event, "service", notification.Service)
	}()
}
