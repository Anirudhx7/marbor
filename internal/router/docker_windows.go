//go:build windows

package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// discoverDockerNodes on Windows connects to Docker Desktop's TCP endpoint
// (localhost:2375). This requires "Expose daemon on tcp://localhost:2375 without
// TLS" to be enabled in Docker Desktop Settings > General.
//
// Named-pipe support (//./pipe/docker_engine) requires the go-winio library
// which is not included to keep the binary dependency-free. If the TCP endpoint
// is not enabled, discovery returns an error that discoverAndAddDockerNodes
// swallows silently  --  disable docker.enabled in config to suppress the attempt.
func discoverDockerNodes(_ string) ([]config.NodeConfig, error) {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{},
	}

	const listURL = `http://localhost:2375/containers/json?filters=%7B%22status%22%3A%5B%22running%22%5D%7D`
	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker discovery: build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker discovery (Windows): connect to localhost:2375: %w  --  enable \"Expose daemon on tcp://localhost:2375\" in Docker Desktop settings, or set docker.enabled: false", err)
	}
	defer resp.Body.Close()

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("docker discovery: decode response: %w", err)
	}

	return parseDockerContainers(containers), nil
}
