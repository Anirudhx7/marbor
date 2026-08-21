//go:build !windows

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// discoverDockerNodes connects to the Docker unix socket and returns NodeConfigs
// for any running Ollama containers. No external dependencies required on
// Linux/macOS - stdlib HTTP over the unix socket is sufficient.
func discoverDockerNodes(socketPath string) ([]config.NodeConfig, error) {
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}

	const listURL = `http://localhost/containers/json?filters=%7B%22status%22%3A%5B%22running%22%5D%7D`
	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker discovery: build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker discovery: connect to %s: %w", socketPath, err)
	}
	defer resp.Body.Close()

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("docker discovery: decode response: %w", err)
	}

	return parseDockerContainers(containers), nil
}
