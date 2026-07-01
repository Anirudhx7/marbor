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
	// NetworkSettings carries the container's per-network IP addresses as
	// reported by the Docker API's list endpoint. This is populated for
	// containers on bridge/custom networks; it is empty for containers
	// running with --network host (they share the host's network
	// namespace and have no container-private IP).
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
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
			URL:      fmt.Sprintf("http://%s:%d", containerHost(c), port),
			GPUModel: "docker",
		})
	}
	return nodes
}

// containerHost returns the address ollama-mesh should use to reach the
// container: the container's own IP address on the first Docker network it
// reports, if one is present. This is correct whether ollama-mesh runs on
// bare metal (routable via the bridge network) or inside another container
// on the same Docker network (container-to-container traffic on a bridge
// network uses container IPs, not 127.0.0.1).
//
// Falls back to 127.0.0.1 only when no container IP can be determined —
// e.g. the container was started with --network host, where it shares the
// host's network namespace and the mapped port is genuinely reachable via
// loopback. We do not attempt to detect ollama-mesh's own network mode here;
// this is a best-effort choice based only on what the Docker API reports for
// the discovered container, not a guarantee of reachability in every topology.
func containerHost(c dockerContainer) string {
	for _, net := range c.NetworkSettings.Networks {
		if net.IPAddress != "" {
			return net.IPAddress
		}
	}
	return "127.0.0.1"
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
