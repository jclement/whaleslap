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
	"github.com/docker/docker/api/types/network"
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

// LogAuthStatus logs the current authentication configuration for debugging
func (c *Client) LogAuthStatus() {
	// Check GITHUB_PAT
	if c.cfg.GithubPAT != "" {
		logger.Info("registry auth: GITHUB_PAT configured", "length", len(c.cfg.GithubPAT))
	} else {
		logger.Info("registry auth: GITHUB_PAT not set")
	}

	// Check Docker config files
	configPaths := []string{
		"/root/.docker/config.json",
		os.Getenv("HOME") + "/.docker/config.json",
	}

	for _, configPath := range configPaths {
		data, err := os.ReadFile(configPath)
		if err != nil {
			logger.Debug("registry auth: docker config not found", "path", configPath)
			continue
		}

		var config struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}

		if err := json.Unmarshal(data, &config); err != nil {
			logger.Warn("registry auth: failed to parse docker config", "path", configPath, "error", err)
			continue
		}

		registries := make([]string, 0, len(config.Auths))
		for registry := range config.Auths {
			registries = append(registries, registry)
		}
		logger.Info("registry auth: docker config found", "path", configPath, "registries", registries)
	}
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

	logger.Debug("resolving auth for image", "image", imageName, "registry", registryHost)

	// Check cache first
	if auth, ok := c.authCache[registryHost]; ok {
		logger.Debug("using cached auth", "registry", registryHost)
		return auth
	}

	// Try GITHUB_PAT for GHCR
	if strings.HasPrefix(imageName, "ghcr.io/") && c.cfg.GithubPAT != "" {
		logger.Debug("using GITHUB_PAT for GHCR auth", "registry", registryHost)
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

	logger.Warn("no auth found for registry", "registry", registryHost, "image", imageName)
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
			logger.Debug("docker config not readable", "path", configPath, "error", err)
			continue
		}

		var config struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}

		if err := json.Unmarshal(data, &config); err != nil {
			logger.Debug("docker config parse error", "path", configPath, "error", err)
			continue
		}

		// Log available registries for debugging
		availableRegistries := make([]string, 0, len(config.Auths))
		for reg := range config.Auths {
			availableRegistries = append(availableRegistries, reg)
		}
		logger.Debug("docker config registries", "path", configPath, "registries", availableRegistries, "looking_for", registryHost)

		// Try exact match first
		if authEntry, ok := config.Auths[registryHost]; ok && authEntry.Auth != "" {
			logger.Debug("using auth from docker config (exact match)", "registry", registryHost, "path", configPath)
			return authEntry.Auth
		}

		// Try with https:// prefix
		if authEntry, ok := config.Auths["https://"+registryHost]; ok && authEntry.Auth != "" {
			logger.Debug("using auth from docker config (https prefix)", "registry", registryHost, "path", configPath)
			return authEntry.Auth
		}
	}

	logger.Debug("no matching auth in docker config", "registry", registryHost)
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

// RecreateContainer stops the old container and creates a new one with the same config but new image
// Preserves all configuration: networking, mounts, environment, labels, etc.
func (c *Client) RecreateContainer(ctx context.Context, serviceName, newImage string) error {
	logger.Info("recreating container", "service", serviceName, "image", newImage)

	// Find the container by compose service label
	containers, err := c.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "com.docker.compose.service="+serviceName),
		),
	})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return fmt.Errorf("container for service %q not found", serviceName)
	}

	oldContainer := containers[0]
	oldContainerID := oldContainer.ID

	// Get full container config
	inspect, err := c.docker.ContainerInspect(ctx, oldContainerID)
	if err != nil {
		return fmt.Errorf("inspecting container: %w", err)
	}

	// Preserve the container name
	containerName := strings.TrimPrefix(oldContainer.Names[0], "/")

	// Build networking config from the old container's network settings
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks != nil {
		for netName, netSettings := range inspect.NetworkSettings.Networks {
			networkingConfig.EndpointsConfig[netName] = &network.EndpointSettings{
				IPAMConfig:          netSettings.IPAMConfig,
				Links:               netSettings.Links,
				Aliases:             netSettings.Aliases,
				MacAddress:          netSettings.MacAddress,
				DriverOpts:          netSettings.DriverOpts,
				NetworkID:           netSettings.NetworkID,
				EndpointID:          "", // Will be assigned by Docker
				Gateway:             "", // Will be assigned by Docker
				IPAddress:           "", // Will be assigned by Docker (unless static)
				IPPrefixLen:         0,  // Will be assigned by Docker
				IPv6Gateway:         "", // Will be assigned by Docker
				GlobalIPv6Address:   "", // Will be assigned by Docker
				GlobalIPv6PrefixLen: 0,  // Will be assigned by Docker
			}
			// Preserve static IP if configured via IPAM
			if netSettings.IPAMConfig != nil {
				networkingConfig.EndpointsConfig[netName].IPAMConfig = netSettings.IPAMConfig
			}
		}
	}

	// Stop the old container
	logger.Debug("stopping container", "container", containerName)
	if err := c.docker.ContainerStop(ctx, oldContainerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("stopping container: %w", err)
	}

	// Remove the old container
	logger.Debug("removing container", "container", containerName)
	if err := c.docker.ContainerRemove(ctx, oldContainerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("removing container: %w", err)
	}

	// Create new container config with updated image
	newConfig := inspect.Config
	newConfig.Image = newImage

	// Create the new container with full config preservation
	resp, err := c.docker.ContainerCreate(ctx, newConfig, inspect.HostConfig, networkingConfig, nil, containerName)
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	logger.Debug("created new container", "container", containerName, "id", resp.ID[:12])

	// Start the new container
	if err := c.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	logger.Info("container recreated successfully", "container", containerName, "image", newImage)
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

// GetContainerImage returns the image name for a container by compose service name
func (c *Client) GetContainerImage(ctx context.Context, serviceName string) (string, error) {
	// Use label-based lookup for exact matching (avoids substring matches)
	containers, err := c.docker.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "com.docker.compose.service="+serviceName),
		),
	})
	if err != nil {
		return "", fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return "", fmt.Errorf("container for service %q not found", serviceName)
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

	// Find all running containers in the same project
	allContainers, err := c.docker.ContainerList(ctx, container.ListOptions{All: false})
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
	info, err := c.getOwnContainerInfo(ctx)
	if err != nil {
		return "", nil // Not in a compose stack
	}
	return info.Config.Labels["com.docker.compose.project"], nil
}

// GetOwnImage returns the image name of the whaleslap container itself
func (c *Client) GetOwnImage(ctx context.Context) (string, error) {
	info, err := c.getOwnContainerInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("getting own container info: %w", err)
	}
	return info.Config.Image, nil
}

// GetOwnServiceName returns the compose service name of the whaleslap container
func (c *Client) GetOwnServiceName(ctx context.Context) (string, error) {
	info, err := c.getOwnContainerInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("getting own container info: %w", err)
	}
	serviceName := info.Config.Labels["com.docker.compose.service"]
	if serviceName == "" {
		return "", fmt.Errorf("container has no compose service label")
	}
	return serviceName, nil
}

// getOwnContainerInfo inspects our own container
func (c *Client) getOwnContainerInfo(ctx context.Context) (container.InspectResponse, error) {
	// Get our hostname which is typically the container ID
	hostname, err := os.Hostname()
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("getting hostname: %w", err)
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
			return container.InspectResponse{}, fmt.Errorf("could not find own container")
		}
		return c.docker.ContainerInspect(ctx, containers[0].ID)
	}

	return info, nil
}
