//go:build windows

package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// dockerTCPEndpoint is the fallback used when socketPath doesn't specify a
// custom tcp:// endpoint - Docker Desktop's default TCP listener when
// "Expose daemon on tcp://localhost:2375 without TLS" is enabled.
const dockerTCPEndpoint = "localhost:2375"

// discoverDockerNodes on Windows connects to a Docker daemon's TCP endpoint.
// socketPath, when set to a "tcp://host:port" value (config's docker.socket),
// is honored; otherwise falls back to Docker Desktop's default
// (localhost:2375) - previously socketPath was silently discarded here
// regardless of what was configured, so a custom TCP endpoint failed
// discovery every cycle with no error.
//
// Named-pipe support (//./pipe/docker_engine) requires the go-winio library
// which is not included to keep the binary dependency-free. If the TCP endpoint
// is not enabled, discovery returns an error that discoverAndAddDockerNodes
// swallows silently - disable docker.enabled in config to suppress the attempt.
func discoverDockerNodes(socketPath string) ([]config.NodeConfig, error) {
	endpoint := dockerTCPEndpoint
	if hostPort, ok := strings.CutPrefix(socketPath, "tcp://"); ok && hostPort != "" {
		endpoint = hostPort
	}

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{},
	}

	listURL := fmt.Sprintf(`http://%s/containers/json?filters=%%7B%%22status%%22%%3A%%5B%%22running%%22%%5D%%7D`, endpoint)
	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker discovery: build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker discovery (Windows): connect to %s: %w - enable \"Expose daemon on tcp://localhost:2375\" in Docker Desktop settings (or configure docker.socket to a custom tcp:// endpoint), or set docker.enabled: false", endpoint, err)
	}
	defer resp.Body.Close()

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("docker discovery: decode response: %w", err)
	}

	return parseDockerContainers(containers), nil
}
