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
		// No agent configured (or deliberately disabled) is not a failure -
		// never fire agent_down for it, and drop any stale prior state so a
		// later re-enable doesn't fire a spurious transition based on
		// whatever the agent's state was before it was disabled.
		r.mu.Lock()
		delete(r.prevAgentPresent, n.Name)
		r.mu.Unlock()
		return
	}

	agentURL, err := buildAgentURL(nodeURL, cfg.Port)
	if err != nil {
		r.agentUnreachable(n, nodeURL)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURL, nil)
	if err != nil {
		r.agentUnreachable(n, nodeURL)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := r.client.Do(req)
	if err != nil {
		r.agentUnreachable(n, nodeURL)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.agentUnreachable(n, nodeURL)
		return
	}

	var t nodeagent.Telemetry
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		r.agentUnreachable(n, nodeURL)
		return
	}

	n.mu.Lock()
	n.AgentFailures = 0
	n.AgentPresent = true
	n.AgentNodeID = t.Agent.NodeID
	n.AgentVersion = t.Agent.Version
	n.AgentCapabilities = append([]string(nil), t.Capabilities...)
	n.AgentPlatform = t.Agent.Platform
	n.AgentArchitecture = t.Agent.Architecture
	// Rolling-upgrade visibility only - decoding above already works
	// regardless of protocol_version (the protocol is additive-only: unknown
	// fields are silently ignored by encoding/json's default Decode, and
	// every field already treats its own zero value as "unknown", not a
	// measurement). This just tells an operator when an agent build is
	// ahead of what this mesh binary was compiled understanding, in case a
	// future genuinely-breaking protocol bump ever needs to be diagnosed -
	// it never gates or changes any decode/routing behavior itself. Logged
	// once per node, not every poll cycle.
	if t.Agent.ProtocolVersion > nodeagent.ProtocolVersion && !n.agentProtocolWarned {
		n.agentProtocolWarned = true
		log.Printf("node %s: agent reports /v1/status protocol_version %d, newer than this mesh understands (%d) - some new agent fields may not be recognized until the mesh is upgraded", n.Name, t.Agent.ProtocolVersion, nodeagent.ProtocolVersion)
	}
	if t.Host != nil {
		n.CPUPercent = derefOr(t.Host.CPUPercent, n.CPUPercent)
		n.RAMUsedMB = t.Host.RAMUsedMB
		n.DiskFreeGB = t.Host.DiskFreeGB
		n.RAMTotalMB = t.Host.RAMTotalMB
		n.DiskTotalGB = t.Host.DiskTotalGB
		n.Hostname = t.Host.Hostname
		n.UptimeSeconds = t.Host.UptimeSeconds
		n.BootTime = t.Host.BootTime
	} else {
		n.RAMUsedMB = 0
		n.DiskFreeGB = 0
		n.RAMTotalMB = 0
		n.DiskTotalGB = 0
		n.Hostname = ""
		n.UptimeSeconds = 0
		n.BootTime = 0
	}
	if t.GPU != nil {
		n.AgentGPUVendor = t.GPU.Vendor
		n.AgentGPUCount = t.GPU.Count
		n.AgentGPUs = append([]nodeagent.GPUInfo(nil), t.GPU.Devices...)
		n.DriverVersion = t.GPU.DriverVersion
		n.CUDAVersion = t.GPU.CUDAVersion
		// FanPercent/Temperature/PowerDrawW/VRAM* fall back to the primary
		// (device 0) reading for the mesh's own single-value routing/UI
		// fields, which predate the multi-GPU array - a node with more than
		// one GPU still surfaces its full per-device breakdown via
		// AgentGPUs, just not through these aggregate-shaped fields.
		if len(t.GPU.Devices) > 0 {
			primary := t.GPU.Devices[0]
			// Auto-fill the node's display name from the card's own reported
			// product name the first time the agent sees one - but only while
			// the field is still empty or the UI's literal "Unknown GPU"
			// placeholder (see admin.go handlePatchNode / GPUNodes.tsx), never
			// overwriting a name an operator deliberately typed or PATCHed in.
			if primary.Model != "" && (n.GPUModel == "" || n.GPUModel == "Unknown GPU") {
				n.GPUModel = primary.Model
			}
			n.FanPercent = primary.FanPercent
			// Only let agent-reported GPU figures override Temperature/
			// PowerDrawW/VRAM* when this node isn't already sourcing richer
			// data from the mesh host's OWN local nvidia-smi (hasGPU) - a
			// remote node with an agent is exactly the case this exists for;
			// a local node polling its own nvidia-smi twice via two
			// different paths would be the "two disagreeing telemetry
			// pipelines" failure mode the build spec warns against (State
			// Hierarchy: one live source, not two).
			if !hasGPU {
				n.Temperature = primary.TemperatureC
				if primary.PowerWatts != nil {
					n.PowerDrawW = *primary.PowerWatts
				}
				if primary.VRAMTotalMB > 0 || primary.VRAMUsedMB > 0 {
					n.VRAMTotalMB = primary.VRAMTotalMB
					n.VRAMUsedMB = primary.VRAMUsedMB
					n.VRAMSource = "agent"
				}
			}
		} else {
			// GPU vendor is known (a backend is selected on this node) but this
			// cycle's Collect() failed - a transient nvidia-smi hiccup, not a
			// permanent "no GPU" state. Clear every per-cycle reading derived
			// from a device, the same way clearAgentTelemetry's wasAgentSourced
			// branch does, so a stale VRAM/temperature/power figure from the
			// last good poll never keeps displaying as current (R1) just
			// because AgentPresent is still true.
			n.FanPercent = nil
			if !hasGPU {
				n.Temperature = nil
				n.PowerDrawW = 0
				if n.VRAMSource == "agent" {
					if n.VRAMTotalMBConfig > 0 {
						n.VRAMTotalMB = n.VRAMTotalMBConfig
						n.VRAMSource = "declared"
					} else {
						n.VRAMTotalMB = 0
						n.VRAMSource = "none"
					}
					n.VRAMUsedMB = 0
				}
			}
		}
	} else {
		n.AgentGPUVendor = ""
		n.AgentGPUCount = 0
		n.AgentGPUs = nil
		n.DriverVersion = ""
		n.CUDAVersion = ""
		n.FanPercent = nil
	}
	if t.Runtime != nil {
		n.AgentRuntime = t.Runtime.Name
		n.RuntimeVersion = t.Runtime.Version
		n.RuntimeStatus = t.Runtime.Status
	} else {
		n.AgentRuntime = ""
		n.RuntimeVersion = ""
		n.RuntimeStatus = ""
	}
	n.mu.Unlock()
	r.agentReachable(n.Name, nodeURL)
}

// agentUnreachable records a failed agent poll and, once AgentFailures
// crosses r.healthFailureThreshold (the same consecutive-failure hysteresis
// pollNode/markFailure use for the node's own inference-runtime health),
// clears the node's agent-derived telemetry and, if this is a genuine down
// transition (the agent was previously confirmed reachable), fires an
// "agent_down" webhook - the same reuse of the existing node_up/node_down
// webhook mechanism (fireWebhook, router.go). A single dropped poll (one TCP
// blip, one timeout) below that threshold intentionally leaves the last-known
// telemetry in place rather than blanking the dashboard for one cycle.
func (r *Router) agentUnreachable(n *NodeState, nodeURL string) {
	n.mu.Lock()
	n.AgentFailures++
	crossedThreshold := n.AgentFailures >= r.healthFailureThreshold
	n.mu.Unlock()
	if !crossedThreshold {
		return
	}

	clearAgentTelemetry(n)
	r.mu.Lock()
	nodeName := n.Name
	exists := r.nodeExistsLocked(nodeName)
	prev, seen := r.prevAgentPresent[nodeName]
	if exists {
		r.prevAgentPresent[nodeName] = false
	}
	r.mu.Unlock()
	if exists && seen && prev {
		r.fireWebhook("agent_down", nodeName, nodeURL)
	}
}

// agentReachable resets the consecutive-failure counter, records a
// successful agent poll and, if this is a recovery (the agent was previously
// down, not merely never-before-seen), fires an "agent_up" webhook. Mirrors
// pollNode's node_up gate: a node's very first successful poll is never
// treated as a "recovery."
func (r *Router) agentReachable(nodeName, nodeURL string) {
	r.mu.Lock()
	exists := r.nodeExistsLocked(nodeName)
	prev, seen := r.prevAgentPresent[nodeName]
	if exists {
		r.prevAgentPresent[nodeName] = true
	}
	r.mu.Unlock()
	if exists && seen && !prev {
		r.fireWebhook("agent_up", nodeName, nodeURL)
	}
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
	n.AgentFailures = 0
	n.AgentPresent = false
	n.AgentNodeID = ""
	n.AgentVersion = ""
	n.AgentCapabilities = nil
	n.AgentPlatform = ""
	n.AgentArchitecture = ""
	n.AgentGPUVendor = ""
	n.AgentGPUCount = 0
	n.AgentGPUs = nil
	n.DriverVersion = ""
	n.CUDAVersion = ""
	n.AgentRuntime = ""
	n.RuntimeVersion = ""
	n.RuntimeStatus = ""
	n.FanPercent = nil
	n.RAMUsedMB = 0
	n.DiskFreeGB = 0
	n.RAMTotalMB = 0
	n.DiskTotalGB = 0
	n.Hostname = ""
	n.UptimeSeconds = 0
	n.BootTime = 0
	// CPUPercent (agent_poll.go's success path, above) is the only writer of
	// NodeState.CPUPercent anywhere in the codebase - reset it here too, or a
	// disabled/unreachable agent's last-reported CPU reading would linger
	// forever with nothing to mark it stale (R1).
	n.CPUPercent = 0
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

// buildAgentURL derives the agent's /v1/status URL from the node's own URL
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
	return fmt.Sprintf("%s://%s:%d/v1/status", scheme, u.Hostname(), port), nil
}
