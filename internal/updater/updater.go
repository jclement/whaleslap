package updater

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/docker"
	"github.com/jclement/whaleslap/internal/logger"
)

type Updater struct {
	cfg    *config.Config
	docker *docker.Client
}

func New(cfg *config.Config, docker *docker.Client) *Updater {
	return &Updater{
		cfg:    cfg,
		docker: docker,
	}
}

// CheckForUpdate checks if an update is available for a service
func (u *Updater) CheckForUpdate(ctx context.Context, serviceName string) (bool, error) {
	logger.Debug("checking for update", "service", serviceName)

	// Look up the image from the running container
	imageName, err := u.docker.GetContainerImage(ctx, serviceName)
	if err != nil {
		return false, fmt.Errorf("getting container image: %w", err)
	}

	hasUpdate, _, err := u.docker.CheckForUpdate(ctx, imageName)
	if err != nil {
		return false, fmt.Errorf("checking for update: %w", err)
	}

	return hasUpdate, nil
}

// ApplyUpdate applies an update to a service
func (u *Updater) ApplyUpdate(ctx context.Context, serviceName string) error {
	logger.Info("applying update", "service", serviceName)

	// Look up the image from the running container
	imageName, err := u.docker.GetContainerImage(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("getting container image: %w", err)
	}

	// Pull the latest image first
	_, _, err = u.docker.CheckForUpdate(ctx, imageName)
	if err != nil {
		return fmt.Errorf("pulling image: %w", err)
	}

	// Use docker compose to recreate the container
	if err := u.composeUp(ctx, serviceName); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}

	logger.Info("update applied successfully", "service", serviceName)
	return nil
}

// composeUp runs docker compose up for a specific service
func (u *Updater) composeUp(ctx context.Context, serviceName string) error {
	composeFile := u.findComposeFile()
	if composeFile == "" {
		return fmt.Errorf("no compose file found - WhaleSlap requires docker compose")
	}

	args := []string{"-f", composeFile}

	if u.cfg.ComposeProject != "" {
		args = append(args, "-p", u.cfg.ComposeProject)
	}

	args = append(args, "up", "-d", "--force-recreate", "--pull", "always", serviceName)

	logger.Debug("running docker compose", "args", args)

	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	return nil
}

func (u *Updater) findComposeFile() string {
	// Check if COMPOSE_FILE env is set first
	if cf := os.Getenv("COMPOSE_FILE"); cf != "" {
		return cf
	}

	// Check common locations
	candidates := []string{
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	}

	// Check current directory
	cwd, _ := os.Getwd()
	for _, name := range candidates {
		path := filepath.Join(cwd, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// SelfUpdate checks if WhaleSlap itself needs updating
func (u *Updater) SelfUpdate(ctx context.Context) error {
	selfImage := os.Getenv("WHALESLAP_SELF_IMAGE")
	if selfImage == "" {
		selfImage = "ghcr.io/jclement/whaleslap:latest"
	}

	logger.Info("checking for self-update", "image", selfImage)

	hasUpdate, _, err := u.docker.CheckForUpdate(ctx, selfImage)
	if err != nil {
		return fmt.Errorf("checking for self-update: %w", err)
	}

	if !hasUpdate {
		logger.Info("whaleslap is up to date")
		return nil
	}

	logger.Info("whaleslap update available, restarting to apply")

	// Find our own service name from hostname or env
	serviceName := os.Getenv("WHALESLAP_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "whaleslap"
	}

	// Use compose to recreate ourselves
	return u.composeUp(ctx, serviceName)
}
