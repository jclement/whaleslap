package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
	"github.com/jclement/whaleslap/internal/notifier"
)

// UpdateFunc applies an update to a container (service name only, image looked up)
type UpdateFunc func(ctx context.Context, serviceName string) error

// CheckFunc checks if an update is available for a service
type CheckFunc func(ctx context.Context, serviceName string) (bool, error)

type PendingUpdate struct {
	ServiceName string
	DetectedAt  time.Time
}

type Scheduler struct {
	cfg            *config.Config
	checkFunc      CheckFunc
	updateFunc     UpdateFunc
	notifier       *notifier.Notifier
	pendingUpdates map[string]*PendingUpdate
	mu             sync.Mutex
	stopCh         chan struct{}
	wg             sync.WaitGroup
	lastCheck      time.Time
	interval       time.Duration
}

func New(cfg *config.Config, checkFunc CheckFunc, updateFunc UpdateFunc) *Scheduler {
	return &Scheduler{
		cfg:            cfg,
		checkFunc:      checkFunc,
		updateFunc:     updateFunc,
		notifier:       notifier.New(cfg),
		pendingUpdates: make(map[string]*PendingUpdate),
		stopCh:         make(chan struct{}),
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	if s.cfg.Schedule == "" {
		logger.Info("no schedule configured, running in webhook-only mode")
		return nil
	}

	interval, err := parseSchedule(s.cfg.Schedule)
	if err != nil {
		return fmt.Errorf("parsing schedule: %w", err)
	}

	s.interval = interval
	logger.Info("starting scheduler", "interval", interval)

	s.wg.Add(1)
	go s.run(ctx, interval)

	// Also start the upgrade window checker if configured
	if s.cfg.UpgradeWindow != "" {
		s.wg.Add(1)
		go s.runUpgradeWindowChecker(ctx)
	}

	return nil
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context, interval time.Duration) {
	defer s.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately on start
	s.lastCheck = time.Now()
	s.checkAllContainers(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.lastCheck = time.Now()
			s.checkAllContainers(ctx)
		}
	}
}

func (s *Scheduler) runUpgradeWindowChecker(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.processPendingUpdates(ctx)
		}
	}
}

func (s *Scheduler) checkAllContainers(ctx context.Context) {
	logger.Info("checking for container updates")

	for _, serviceName := range s.cfg.Containers {
		hasUpdate, err := s.checkFunc(ctx, serviceName)
		if err != nil {
			logger.Error("failed to check for updates", "service", serviceName, "error", err)
			continue
		}

		if hasUpdate {
			s.QueueUpdate(serviceName)
		}
	}

	// Try to process immediately if in window
	s.processPendingUpdates(ctx)
}

// QueueUpdate adds an update to the pending queue
func (s *Scheduler) QueueUpdate(serviceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.pendingUpdates[serviceName]; exists {
		logger.Debug("update already pending", "service", serviceName)
		return
	}

	s.pendingUpdates[serviceName] = &PendingUpdate{
		ServiceName: serviceName,
		DetectedAt:  time.Now(),
	}

	logger.Info("update queued", "service", serviceName)
	s.notifier.Notify(context.Background(), notifier.EventUpdateQueued, serviceName, "Update detected and queued")
}

func (s *Scheduler) processPendingUpdates(ctx context.Context) {
	if !s.cfg.IsInUpgradeWindow() {
		logger.Debug("not in upgrade window, skipping updates")
		return
	}

	s.mu.Lock()
	updates := make([]*PendingUpdate, 0, len(s.pendingUpdates))
	for _, update := range s.pendingUpdates {
		updates = append(updates, update)
	}
	s.mu.Unlock()

	for _, update := range updates {
		// Check if update is actually available (pulls latest and compares digests)
		hasUpdate, err := s.checkFunc(ctx, update.ServiceName)
		if err != nil {
			logger.Error("failed to check for update", "service", update.ServiceName, "error", err)
			s.mu.Lock()
			delete(s.pendingUpdates, update.ServiceName)
			s.mu.Unlock()
			continue
		}

		if !hasUpdate {
			logger.Debug("image is current", "service", update.ServiceName)
			s.mu.Lock()
			delete(s.pendingUpdates, update.ServiceName)
			s.mu.Unlock()
			continue
		}

		logger.Info("applying update", "service", update.ServiceName)
		s.notifier.Notify(ctx, notifier.EventUpdateStarted, update.ServiceName, "Starting container update")

		if err := s.updateFunc(ctx, update.ServiceName); err != nil {
			logger.Error("failed to apply update", "service", update.ServiceName, "error", err)
			s.notifier.NotifyError(ctx, notifier.EventUpdateFailed, update.ServiceName, err)
			continue
		}

		s.mu.Lock()
		delete(s.pendingUpdates, update.ServiceName)
		s.mu.Unlock()

		logger.Info("update applied successfully", "service", update.ServiceName)
		s.notifier.Notify(ctx, notifier.EventUpdateCompleted, update.ServiceName, "Container updated successfully")
	}
}

// TriggerUpdate is called by webhooks to immediately queue an update
func (s *Scheduler) TriggerUpdate(serviceName string) {
	s.QueueUpdate(serviceName)

	// Try to process immediately
	go s.processPendingUpdates(context.Background())
}

// GetPendingUpdates returns the current pending updates
func (s *Scheduler) GetPendingUpdates() []*PendingUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()

	updates := make([]*PendingUpdate, 0, len(s.pendingUpdates))
	for _, update := range s.pendingUpdates {
		updates = append(updates, update)
	}
	return updates
}

// IsConfigured checks if a service is in our configuration
func (s *Scheduler) IsConfigured(serviceName string) bool {
	for _, name := range s.cfg.Containers {
		if name == serviceName {
			return true
		}
	}
	return false
}

// GetNextCheckTime returns the time of the next scheduled check
func (s *Scheduler) GetNextCheckTime() string {
	if s.interval == 0 {
		return ""
	}
	nextCheck := s.lastCheck.Add(s.interval)
	remaining := time.Until(nextCheck)
	if remaining < 0 {
		return "now"
	}
	return fmt.Sprintf("in %s", remaining.Round(time.Second))
}

// parseSchedule parses a schedule string into a duration
// Supports: "5m", "1h", "30s", or cron-like "every 5 minutes"
func parseSchedule(schedule string) (time.Duration, error) {
	schedule = strings.TrimSpace(strings.ToLower(schedule))

	// Try parsing as duration first
	if d, err := time.ParseDuration(schedule); err == nil {
		return d, nil
	}

	// Try parsing human-readable formats
	patterns := map[*regexp.Regexp]func([]string) (time.Duration, error){
		regexp.MustCompile(`^every\s+(\d+)\s+minutes?$`): func(m []string) (time.Duration, error) {
			n, _ := strconv.Atoi(m[1])
			return time.Duration(n) * time.Minute, nil
		},
		regexp.MustCompile(`^every\s+(\d+)\s+hours?$`): func(m []string) (time.Duration, error) {
			n, _ := strconv.Atoi(m[1])
			return time.Duration(n) * time.Hour, nil
		},
		regexp.MustCompile(`^hourly$`): func(m []string) (time.Duration, error) {
			return time.Hour, nil
		},
		regexp.MustCompile(`^daily$`): func(m []string) (time.Duration, error) {
			return 24 * time.Hour, nil
		},
	}

	for pattern, parser := range patterns {
		if matches := pattern.FindStringSubmatch(schedule); matches != nil {
			return parser(matches)
		}
	}

	return 0, fmt.Errorf("invalid schedule format: %q", schedule)
}
