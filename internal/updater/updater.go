package updater

import (
	"context"
	"fmt"

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

	// Recreate the container with the new image using Docker API
	if err := u.docker.RecreateContainer(ctx, serviceName, imageName); err != nil {
		return fmt.Errorf("recreating container: %w", err)
	}

	logger.Info("update applied successfully", "service", serviceName)
	return nil
}

// SelfUpdate checks if WhaleSlap itself needs updating
func (u *Updater) SelfUpdate(ctx context.Context) error {
	// Get our own image from the running container
	selfImage, err := u.docker.GetOwnImage(ctx)
	if err != nil {
		return fmt.Errorf("getting own image: %w", err)
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

	// Get our own service name from container labels
	serviceName, err := u.docker.GetOwnServiceName(ctx)
	if err != nil {
		return fmt.Errorf("getting own service name: %w", err)
	}

	// Recreate ourselves with Docker API
	return u.docker.RecreateContainer(ctx, serviceName, selfImage)
}
