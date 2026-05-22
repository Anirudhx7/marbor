package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// dockerContainer is the subset of fields we need from Docker's /containers/json response.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	Ports []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// discoverDockerNodes connects to the Docker socket and returns NodeConfigs for
// any running containers that appear to be Ollama instances.
// It detects Ollama containers by image name containing "ollama".
// No external dependencies — uses stdlib HTTP over the Unix socket.
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

	// Request only running containers.
	// URL-encoded: {"status":["running"]}
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

	var nodes []config.NodeConfig
	for _, c := range containers {
		if !isOllamaContainer(c) {
			continue
		}
		port := findPublicPort(c, 11434)
		if port == 0 {
			continue // port not mapped to host, unreachable
		}
		nodes = append(nodes, config.NodeConfig{
			Name:     containerNodeName(c),
			URL:      fmt.Sprintf("http://127.0.0.1:%d", port),
			GPUModel: "docker",
		})
	}
	return nodes, nil
}

// isOllamaContainer returns true if the container's image name contains "ollama".
func isOllamaContainer(c dockerContainer) bool {
	return strings.Contains(strings.ToLower(c.Image), "ollama")
}

// findPublicPort returns the host-mapped port for the given container private port.
// Returns 0 if no mapping exists.
func findPublicPort(c dockerContainer, privatePort int) int {
	for _, p := range c.Ports {
		if p.PrivatePort == privatePort && p.Type == "tcp" && p.PublicPort > 0 {
			return p.PublicPort
		}
	}
	return 0
}

// containerNodeName derives a clean node name from the container.
func containerNodeName(c dockerContainer) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return "docker-" + c.ID[:12]
	}
	return "docker-" + c.ID
}
