package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/jclement/whaleslap/internal/config"
	"github.com/jclement/whaleslap/internal/logger"
)

type Client struct {
	docker    *client.Client
	cfg       *config.Config
	authCache map[string]string
}

func NewClient(cfg *config.Config) (*Client, error) {
	opts := []client.Opt{
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	}

	if cfg.DockerHost != "" {
		opts = append(opts, client.WithHost(cfg.DockerHost))
	}

	docker, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	return &Client{
		docker:    docker,
		cfg:       cfg,
		authCache: make(map[string]string),
	}, nil
}

func (c *Client) Close() error {
	return c.docker.Close()
}

// CheckForUpdate checks if a new version of the image is available
func (c *Client) CheckForUpdate(ctx context.Context, imageName string) (bool, string, error) {
	logger.Debug("checking for updates", "image", imageName)

	// Get current image digest
	currentDigest, err := c.getLocalImageDigest(ctx, imageName)
	if err != nil {
		logger.Warn("could not get local image digest, will pull", "image", imageName, "error", err)
		return true, "", nil
	}

	// Pull latest image
	auth := c.getAuthForImage(imageName)
	reader, err := c.docker.ImagePull(ctx, imageName, image.PullOptions{
		RegistryAuth: auth,
	})
	if err != nil {
		return false, "", fmt.Errorf("pulling image %s: %w", imageName, err)
	}
	defer reader.Close()

	// Consume the pull output
	output, _ := io.ReadAll(reader)
	logger.Debug("pull output", "image", imageName, "output", string(output))

	// Get new image digest
	newDigest, err := c.getLocalImageDigest(ctx, imageName)
	if err != nil {
		return false, "", fmt.Errorf("getting new image digest: %w", err)
	}

	if currentDigest != newDigest {
		logger.Info("new image version available", "image", imageName, "old", currentDigest[:12], "new", newDigest[:12])
		return true, newDigest, nil
	}

	logger.Debug("image is up to date", "image", imageName)
	return false, currentDigest, nil
}

func (c *Client) getLocalImageDigest(ctx context.Context, imageName string) (string, error) {
	inspect, _, err := c.docker.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return "", err
	}

	if len(inspect.RepoDigests) > 0 {
		// Extract digest from "image@sha256:..."
		parts := strings.SplitN(inspect.RepoDigests[0], "@", 2)
		if len(parts) == 2 {
			return parts[1], nil
		}
	}

	return inspect.ID, nil
}

func (c *Client) getAuthForImage(imageName string) string {
	// Extract registry from image name
	registryHost := "docker.io"
	if strings.Contains(imageName, "/") {
		parts := strings.SplitN(imageName, "/", 2)
		if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
			registryHost = parts[0]
		}
	}

	// Check cache first
	if auth, ok := c.authCache[registryHost]; ok {
		return auth
	}

	// Try GITHUB_PAT for GHCR
	if strings.HasPrefix(imageName, "ghcr.io/") && c.cfg.GithubPAT != "" {
		authConfig := registry.AuthConfig{
			Username: "USERNAME",
			Password: c.cfg.GithubPAT,
		}
		encodedJSON, _ := json.Marshal(authConfig)
		auth := base64.URLEncoding.EncodeToString(encodedJSON)
		c.authCache[registryHost] = auth
		return auth
	}

	// Try to load from Docker config file (~/.docker/config.json)
	auth := c.getAuthFromDockerConfig(registryHost)
	if auth != "" {
		c.authCache[registryHost] = auth
		return auth
	}

	return ""
}

func (c *Client) getAuthFromDockerConfig(registryHost string) string {
	// Check common locations for Docker config
	configPaths := []string{
		"/root/.docker/config.json",
		os.Getenv("HOME") + "/.docker/config.json",
	}

	for _, configPath := range configPaths {
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		var config struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}

		if err := json.Unmarshal(data, &config); err != nil {
			continue
		}

		// Try exact match first
		if authEntry, ok := config.Auths[registryHost]; ok && authEntry.Auth != "" {
			logger.Debug("using auth from docker config", "registry", registryHost)
			return authEntry.Auth
		}

		// Try with https:// prefix
		if authEntry, ok := config.Auths["https://"+registryHost]; ok && authEntry.Auth != "" {
			logger.Debug("using auth from docker config", "registry", registryHost)
			return authEntry.Auth
		}
	}

	return ""
}

// UpdateContainer pulls the latest image and restarts the container
func (c *Client) UpdateContainer(ctx context.Context, containerName, imageName string) error {
	logger.Info("updating container", "container", containerName, "image", imageName)

	// Find the container
	containers, err := c.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("name", containerName),
		),
	})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return fmt.Errorf("container %q not found", containerName)
	}

	target := containers[0]

	// Check if part of compose project
	if c.cfg.ComposeProject != "" {
		if project, ok := target.Labels["com.docker.compose.project"]; ok {
			if project != c.cfg.ComposeProject {
				return fmt.Errorf("container %q is not part of compose project %q", containerName, c.cfg.ComposeProject)
			}
		}
	}

	// Pull the latest image
	auth := c.getAuthForImage(imageName)
	reader, err := c.docker.ImagePull(ctx, imageName, image.PullOptions{
		RegistryAuth: auth,
	})
	if err != nil {
		return fmt.Errorf("pulling image: %w", err)
	}
	io.Copy(io.Discard, reader)
	reader.Close()

	logger.Info("pulled new image", "image", imageName)

	// Stop the container
	logger.Debug("stopping container", "container", containerName)
	if err := c.docker.ContainerStop(ctx, target.ID, container.StopOptions{}); err != nil {
		return fmt.Errorf("stopping container: %w", err)
	}

	// Remove the container
	logger.Debug("removing container", "container", containerName)
	if err := c.docker.ContainerRemove(ctx, target.ID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing container: %w", err)
	}

	// Recreate using docker compose up
	logger.Info("container stopped and removed, use 'docker compose up -d' to recreate", "container", containerName)

	return nil
}

// RecreateContainer uses docker compose to recreate a container with the new image
func (c *Client) RecreateContainer(ctx context.Context, containerName string) error {
	// We'll trigger compose to recreate via labels
	// This is handled by the scheduler/webhook calling docker compose
	return nil
}

// GetContainerInfo returns information about a container
func (c *Client) GetContainerInfo(ctx context.Context, containerName string) (container.InspectResponse, error) {
	return c.docker.ContainerInspect(ctx, containerName)
}

// ListManagedContainers returns containers that match our configuration
func (c *Client) ListManagedContainers(ctx context.Context) ([]container.Summary, error) {
	allContainers, err := c.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	var managed []container.Summary
	configuredNames := make(map[string]bool)
	for _, serviceName := range c.cfg.Containers {
		configuredNames[serviceName] = true
	}

	for _, cont := range allContainers {
		for _, name := range cont.Names {
			// Container names start with /
			cleanName := strings.TrimPrefix(name, "/")
			if configuredNames[cleanName] {
				managed = append(managed, cont)
				break
			}
		}
	}

	return managed, nil
}

// SelfUpdate checks and updates the whaleslap container itself
func (c *Client) SelfUpdate(ctx context.Context, selfImage string) (bool, error) {
	logger.Info("checking for whaleslap self-update", "image", selfImage)

	updated, _, err := c.CheckForUpdate(ctx, selfImage)
	if err != nil {
		return false, fmt.Errorf("checking for self-update: %w", err)
	}

	if !updated {
		logger.Info("whaleslap is up to date")
		return false, nil
	}

	logger.Info("new whaleslap version available, will update on next restart")
	return true, nil
}

// GetContainerImage returns the image name for a running container
func (c *Client) GetContainerImage(ctx context.Context, containerName string) (string, error) {
	containers, err := c.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("name", containerName),
		),
	})
	if err != nil {
		return "", fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return "", fmt.Errorf("container %q not found", containerName)
	}

	return containers[0].Image, nil
}

// ResolveContainerImage returns the image for a container, looking it up if not specified
func (c *Client) ResolveContainerImage(ctx context.Context, containerName, configImage string) (string, error) {
	if configImage != "" {
		return configImage, nil
	}

	// Look up from running container
	imageName, err := c.GetContainerImage(ctx, containerName)
	if err != nil {
		return "", fmt.Errorf("could not determine image for container %q: %w", containerName, err)
	}

	logger.Debug("resolved image from container", "container", containerName, "image", imageName)
	return imageName, nil
}

// DiscoverComposeServices finds all services in the same compose stack as whaleslap
func (c *Client) DiscoverComposeServices(ctx context.Context) ([]string, error) {
	// First, find our own compose project by looking at our container
	ourProject, err := c.getOwnComposeProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting compose project: %w", err)
	}

	// Use configured project if set, otherwise use auto-detected
	targetProject := c.cfg.ComposeProject
	if targetProject == "" {
		targetProject = ourProject
	}

	if targetProject == "" {
		return nil, fmt.Errorf("not running in a compose stack (no com.docker.compose.project label)")
	}

	logger.Debug("discovering services in compose project", "project", targetProject)

	// Find all containers in the same project
	allContainers, err := c.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var services []string
	seen := make(map[string]bool)

	for _, cont := range allContainers {
		// Check if it's in our compose project
		project := cont.Labels["com.docker.compose.project"]
		if project != targetProject {
			continue
		}

		serviceName := cont.Labels["com.docker.compose.service"]
		if serviceName == "" {
			continue
		}

		// Skip whaleslap itself
		if serviceName == "whaleslap" {
			continue
		}

		// Skip services with whaleslap.ignore=true label
		if cont.Labels["whaleslap.ignore"] == "true" {
			logger.Debug("skipping ignored service", "service", serviceName)
			continue
		}

		// Avoid duplicates
		if seen[serviceName] {
			continue
		}
		seen[serviceName] = true

		services = append(services, serviceName)
		logger.Debug("discovered compose service", "service", serviceName)
	}

	return services, nil
}

// getOwnComposeProject finds the compose project of the whaleslap container itself
func (c *Client) getOwnComposeProject(ctx context.Context) (string, error) {
	// Get our hostname which is typically the container ID
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("getting hostname: %w", err)
	}

	// Try to inspect our own container
	info, err := c.docker.ContainerInspect(ctx, hostname)
	if err != nil {
		// Fallback: search for container with whaleslap service label
		containers, err := c.docker.ContainerList(ctx, container.ListOptions{
			All: true,
			Filters: filters.NewArgs(
				filters.Arg("label", "com.docker.compose.service=whaleslap"),
			),
		})
		if err != nil || len(containers) == 0 {
			return "", nil // Not in a compose stack
		}
		return containers[0].Labels["com.docker.compose.project"], nil
	}

	return info.Config.Labels["com.docker.compose.project"], nil
}
