package router

// agent_poll.go - polls a node's Node Agent (internal/nodeagent) for
// GPU/host telemetry on the same poll cycle as /api/ps. Pull-only, per
// .local/specs/node-agent.md section 3: no new transport layer, no
// reconnect/backpressure logic - reuses the same http.Client used for
// everything else in this package.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
)

// pollAgentTelemetry updates n's agent-derived telemetry fields. If no
// agent is configured for n (the common case - agent is opt-in per node),
// it clears any previously-reported agent fields so a node that had its
// agent disabled doesn't keep showing stale data (R1). On any failure to
// reach a configured agent (network error, non-200, bad token, malformed
// body), it likewise clears the fields rather than leaving a stale value -
// AgentPresent is the single source of truth consumers must check before
// trusting FanPercent/RAMUsedMB/DiskFreeGB/AgentVersion.
func (r *Router) pollAgentTelemetry(n *NodeState) {
	n.mu.RLock()
	nodeURL := n.URL
	hasGPU := n.VRAMSource == "nvidia"
	n.mu.RUnlock()

	cfg, ok := r.NodeAgentSetting(n.Name)
	if !ok || !cfg.Enabled {
		clearAgentTelemetry(n)
		return
	}

	agentURL, err := buildAgentURL(nodeURL, cfg.Port)
	if err != nil {
		clearAgentTelemetry(n)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURL, nil)
	if err != nil {
		clearAgentTelemetry(n)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := r.client.Do(req)
	if err != nil {
		clearAgentTelemetry(n)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		clearAgentTelemetry(n)
		return
	}

	var t nodeagent.Telemetry
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		clearAgentTelemetry(n)
		return
	}

	n.mu.Lock()
	n.AgentPresent = true
	n.AgentVersion = t.AgentVersion
	n.AgentCapabilities = append([]string(nil), t.Capabilities...)
	n.AgentPlatform = t.Platform
	n.AgentArchitecture = t.Architecture
	n.AgentGPUVendor = t.GPUVendor
	n.AgentRuntime = t.Runtime
	// Rolling-upgrade visibility only - decoding above already works
	// regardless of schema_version (the protocol is additive-only: unknown
	// fields are silently ignored by encoding/json's default Decode, and
	// every field already treats its own zero value as "unknown", not a
	// measurement). This just tells an operator when an agent build is
	// ahead of what this mesh binary was compiled understanding, in case a
	// future genuinely-breaking schema bump ever needs to be diagnosed - it
	// never gates or changes any decode/routing behavior itself. Logged
	// once per node, not every poll cycle.
	if t.SchemaVersion > nodeagent.SchemaVersion && !n.agentSchemaWarned {
		n.agentSchemaWarned = true
		log.Printf("node %s: agent reports /telemetry schema_version %d, newer than this mesh understands (%d) - some new agent fields may not be recognized until the mesh is upgraded", n.Name, t.SchemaVersion, nodeagent.SchemaVersion)
	}
	if t.Host != nil {
		n.CPUPercent = derefOr(t.Host.CPUPercent, n.CPUPercent)
		n.RAMUsedMB = t.Host.RAMUsedMB
		n.DiskFreeGB = t.Host.DiskFreeGB
	} else {
		n.RAMUsedMB = 0
		n.DiskFreeGB = 0
	}
	if t.GPU != nil {
		n.FanPercent = t.GPU.FanPercent
		// Only let agent-reported GPU figures override Temperature/PowerDrawW/
		// VRAM* when this node isn't already sourcing richer data from the
		// mesh host's OWN local nvidia-smi (hasGPU) - a remote node with an
		// agent is exactly the case this exists for; a local node polling its
		// own nvidia-smi twice via two different paths would be the "two
		// disagreeing telemetry pipelines" failure mode the build spec warns
		// against (State Hierarchy: one live source, not two).
		if !hasGPU {
			n.Temperature = t.GPU.TemperatureC
			if t.GPU.PowerWatts != nil {
				n.PowerDrawW = *t.GPU.PowerWatts
			}
			if t.GPU.VRAMTotalMB > 0 || t.GPU.VRAMUsedMB > 0 {
				n.VRAMTotalMB = t.GPU.VRAMTotalMB
				n.VRAMUsedMB = t.GPU.VRAMUsedMB
				n.VRAMSource = "agent"
			}
		}
	} else {
		n.FanPercent = nil
	}
	n.mu.Unlock()
}

// derefOr returns *p if p is non-nil, else fallback - used to avoid
// clobbering CPUPercent (already populated from other sources for some
// runtimes) with a zero value when the agent didn't report one.
func derefOr(p *float64, fallback float64) float64 {
	if p != nil {
		return *p
	}
	return fallback
}

// clearAgentTelemetry resets every agent-derived field to its zero/unknown
// value. Called whenever no agent is configured for a node, or the most
// recent poll of a configured agent failed.
func clearAgentTelemetry(n *NodeState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wasAgentSourced := n.VRAMSource == "agent"
	n.AgentPresent = false
	n.AgentVersion = ""
	n.AgentCapabilities = nil
	n.AgentPlatform = ""
	n.AgentArchitecture = ""
	n.AgentGPUVendor = ""
	n.AgentRuntime = ""
	n.agentSchemaWarned = false
	n.FanPercent = nil
	n.RAMUsedMB = 0
	n.DiskFreeGB = 0
	if wasAgentSourced {
		// Fall back to whatever the node's declared/API-derived VRAM would
		// otherwise be, same defaulting pollNode's non-local branch uses.
		if n.VRAMTotalMBConfig > 0 {
			n.VRAMTotalMB = n.VRAMTotalMBConfig
			n.VRAMSource = "declared"
		} else {
			n.VRAMTotalMB = 0
			n.VRAMSource = "none"
		}
		n.VRAMUsedMB = 0
		n.Temperature = nil
		n.PowerDrawW = 0
	}
}

// buildAgentURL derives the agent's /telemetry URL from the node's own URL
// (same host) and the configured agent port, via url.Parse per R5 - never
// arithmetic port derivation.
func buildAgentURL(nodeURL string, port int) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", fmt.Errorf("parse node URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("node URL %q has no host", nodeURL)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/telemetry", scheme, u.Hostname(), port), nil
}
