package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/docker"
	"github.com/jclement/whaleslap/internal/logger"
	"github.com/jclement/whaleslap/internal/scheduler"
	"github.com/jclement/whaleslap/internal/updater"
	"github.com/jclement/whaleslap/internal/webhook"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("whaleslap %s (%s) built %s\n", version, commit, buildDate)
		os.Exit(0)
	}

	// Initialize logger
	debugMode := os.Getenv("WHALESLAP_DEBUG") == "true" || os.Getenv("WHALESLAP_DEBUG") == "1"
	logger.Init(debugMode)

	logger.Info("starting whaleslap",
		"version", version,
		"commit", commit,
		"build_date", buildDate)

	// Load configuration from environment variables
	cfg, err := config.Load("")
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	logger.Info("configuration loaded",
		"schedule", cfg.Schedule,
		"upgrade_window", cfg.UpgradeWindow,
		"webhook_id", cfg.WebhookID[:8]+"...")

	// Create Docker client
	dockerClient, err := docker.NewClient(cfg)
	if err != nil {
		logger.Error("failed to create docker client", "error", err)
		os.Exit(1)
	}
	defer dockerClient.Close()

	// Discover services from compose stack
	logger.Info("discovering compose services...")
	services, err := dockerClient.DiscoverComposeServices(context.Background())
	if err != nil {
		logger.Error("failed to discover services", "error", err)
		os.Exit(1)
	}
	if len(services) == 0 {
		logger.Error("no compose services found to manage")
		os.Exit(1)
	}
	cfg.Containers = services
	logger.Info("discovered services", "count", len(services))

	// Create updater
	upd := updater.New(cfg, dockerClient)

	// Create scheduler
	sched := scheduler.New(cfg, upd.CheckForUpdate, upd.ApplyUpdate)

	// Create webhook server
	webhookServer := webhook.NewServer(cfg, sched)

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start scheduler
	if err := sched.Start(ctx); err != nil {
		logger.Error("failed to start scheduler", "error", err)
		os.Exit(1)
	}

	// Start webhook server
	if err := webhookServer.Start(); err != nil {
		logger.Error("failed to start webhook server", "error", err)
		os.Exit(1)
	}

	// Print startup information
	printStartupInfo(cfg, webhookServer)

	// Schedule self-update check (nightly at 3am)
	go scheduleSelfUpdate(ctx, upd)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received shutdown signal", "signal", sig)

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	sched.Stop()
	if err := webhookServer.Stop(shutdownCtx); err != nil {
		logger.Error("error during shutdown", "error", err)
	}

	logger.Info("whaleslap stopped")
}

func printStartupInfo(cfg *config.Config, server *webhook.Server) {
	logger.Info("whaleslap is ready")
	logger.Info("webhook endpoint", "url", server.GetWebhookURL())

	if cfg.Schedule != "" {
		logger.Info("scheduled checks enabled", "schedule", cfg.Schedule)
	} else {
		logger.Info("running in webhook-only mode (no scheduled checks)")
	}

	if cfg.UpgradeWindow != "" {
		logger.Info("upgrade window configured",
			"window", cfg.UpgradeWindow,
			"currently_in_window", cfg.IsInUpgradeWindow())
	}

	logger.Info("managed services:")
	for _, name := range cfg.Containers {
		logger.Info("  - " + name)
	}

	// Print example curl command
	fmt.Println()
	fmt.Println("Example webhook trigger:")
	if len(cfg.Containers) > 0 {
		fmt.Println(webhook.FormatCurlExample("http://localhost:"+fmt.Sprint(cfg.Port), cfg.WebhookID, cfg.Containers[0]))
	}
	fmt.Println()
}

func scheduleSelfUpdate(ctx context.Context, upd *updater.Updater) {
	// Calculate time until 3am
	now := time.Now()
	next3am := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
	if now.After(next3am) {
		next3am = next3am.Add(24 * time.Hour)
	}

	timer := time.NewTimer(time.Until(next3am))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			logger.Info("running nightly self-update check")
			if err := upd.SelfUpdate(ctx); err != nil {
				logger.Error("self-update failed", "error", err)
			}
			// Reset timer for next day
			timer.Reset(24 * time.Hour)
		}
	}
}
