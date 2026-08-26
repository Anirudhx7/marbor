package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Anirudhx7/marbor/internal/config"
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
		host, isContainerIP := containerHost(c)
		// A container's own IP is only reachable at its private (in-namespace)
		// port, never the host's NAT-mapped public port - the two only happen
		// to match under an identity mapping (-p 11434:11434). The looked-up
		// PublicPort is only meaningful for the 127.0.0.1 host-network
		// fallback, where the container shares the host's port space.
		var port int
		if isContainerIP {
			port = 11434
		} else {
			port = findPublicPort(c, 11434)
			if port == 0 {
				// containerHost already returned isContainerIP=false, meaning
				// no network entry had a usable IPAddress - the documented
				// --network host signature (Docker reports a "host" network
				// entry with an empty IPAddress, not an absent Networks map).
				// That container shares the host's network namespace and has
				// no NAT mapping to report, so use the fixed private port
				// directly via the same 127.0.0.1 fallback rather than
				// skipping the container outright.
				port = 11434
			}
		}
		nodes = append(nodes, config.NodeConfig{
			Name:     containerNodeName(c),
			URL:      fmt.Sprintf("http://%s:%d", host, port),
			GPUModel: "docker",
		})
	}
	return nodes
}

// containerHost returns the address marbor should use to reach the
// container: the container's own IP address on the first Docker network it
// reports, if one is present (isContainerIP=true). This is correct whether
// marbor runs on bare metal (routable via the bridge network) or inside
// another container on the same Docker network (container-to-container
// traffic on a bridge network uses container IPs, not 127.0.0.1).
//
// Falls back to 127.0.0.1 (isContainerIP=false) only when no container IP
// can be determined -- e.g. the container was started with --network host,
// where it shares the host's network namespace and the mapped port is
// genuinely reachable via loopback. We do not attempt to detect marbor's own
// network mode here; this is a best-effort choice based only on what the
// Docker API reports for the discovered container, not a guarantee of
// reachability in every topology.
func containerHost(c dockerContainer) (host string, isContainerIP bool) {
	// Iterate networks in sorted name order for a deterministic choice: Go map
	// iteration is randomized, so a multi-network container could otherwise
	// resolve to a different IP on each discovery poll and be registered as two
	// separate nodes over time.
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for name := range c.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ip := c.NetworkSettings.Networks[name].IPAddress; ip != "" {
			return ip, true
		}
	}
	return "127.0.0.1", false
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
