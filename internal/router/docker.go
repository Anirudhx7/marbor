package router

import (
	"fmt"
	"strings"

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

// parseDockerContainers converts a decoded container list to NodeConfig entries.
// Shared between the Unix and Windows discovery implementations.
func parseDockerContainers(containers []dockerContainer) []config.NodeConfig {
	var nodes []config.NodeConfig
	for _, c := range containers {
		if !isOllamaContainer(c) {
			continue
		}
		port := findPublicPort(c, 11434)
		if port == 0 {
			continue
		}
		nodes = append(nodes, config.NodeConfig{
			Name:     containerNodeName(c),
			URL:      fmt.Sprintf("http://127.0.0.1:%d", port),
			GPUModel: "docker",
		})
	}
	return nodes
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
