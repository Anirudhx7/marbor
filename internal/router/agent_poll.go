package router

// agent_poll.go - polls each physical host's Marbor Agent (internal/marboragent)
// exactly once per refresh interval, on its own goroutine group (see
// pollAgentHosts, called alongside - not nested inside - the per-node
// /api/ps health poll in health.go). One poll's Telemetry is fanned out to
// every NodeState that shares that host (see NodeState.Host), so a
// multi-runtime box (e.g. Ollama on :11434 and vLLM on :8000) needs exactly
// one agent process/enrollment/token and exactly one HTTP request per tick,
// never N. Pull-only: no new transport layer, no reconnect/backpressure
// logic - reuses the same http.Client used for everything else in this
// package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// pollAgentHosts groups every current node by its shared Host, polls each
// enabled host's agent exactly once, and fans the result out via
// pollAgentHost. Any node whose host has no enabled agent configured gets
// its stale telemetry cleared here too, same as the old per-node "no agent
// configured" branch.
func (r *Router) pollAgentHosts() {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	marborAgentsSnapshot := make(map[string]MarborAgentConfig, len(r.marborAgents))
	for h, cfg := range r.marborAgents {
		marborAgentsSnapshot[h] = cfg
	}
	r.mu.RUnlock()

	groups := make(map[string][]*NodeState)
	for _, n := range nodes {
		n.RLock()
		host := n.Host
		n.RUnlock()
		if cfg, ok := marborAgentsSnapshot[host]; ok && cfg.Enabled {
			groups[host] = append(groups[host], n)
			continue
		}
		// No agent configured (or deliberately disabled) for this node's
		// host - not a failure, never fires agent_down for it, and drops any
		// stale prior state so a later re-enable doesn't fire a spurious
		// transition based on whatever the agent's state was before it was
		// disabled.
		clearAgentTelemetry(n)
		r.setAgentStale(n, false)
		r.mu.Lock()
		delete(r.prevAgentPresent, n.Name)
		r.mu.Unlock()
	}

	var wg sync.WaitGroup
	for host, members := range groups {
		cfg := marborAgentsSnapshot[host]
		wg.Add(1)
		go func(host string, cfg MarborAgentConfig, members []*NodeState) {
			defer wg.Done()
			r.pollAgentHost(host, cfg, members)
		}(host, cfg, members)
	}
	wg.Wait()
}

// pollAgentHost makes the single HTTP request for host and applies the
// result to every member sharing it - host telemetry/GPU/capabilities
// identically to each, runtime-specific fields (AgentRuntime/RuntimeVersion/
// RuntimeStatus/AgentRuntimeID) matched per member against the polled
// Telemetry.Runtimes array. On any failure to reach the agent, every member
// is cleared exactly like the old single-node failure path.
func (r *Router) pollAgentHost(host string, cfg MarborAgentConfig, members []*NodeState) {
	if len(members) == 0 {
		return
	}

	// scheme is the agent's OWN transport scheme (cfg.Scheme) - independent
	// of any member's runtime URL scheme. Marbor Agent URL construction used
	// to derive this from the runtime URL instead, which meant enabling
	// HTTPS for the agent also silently switched the runtime endpoint to
	// https:// and broke runtimes that only serve plain HTTP. See
	// store.MarborAgentRecord.Scheme's doc comment.
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "http"
	}

	agentURL, err := buildAgentURL(host, cfg.Port, scheme)
	if err != nil {
		for _, n := range members {
			r.agentUnreachable(n)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURL, nil)
	if err != nil {
		for _, n := range members {
			r.agentUnreachable(n)
		}
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	// r.client's Transport is the Router's single shared, TLS-pinning-aware
	// Transport (see tls_dial.go, HTTPClientForNode) - an https:// agentURL
	// here is verified against this host's pinned fingerprint exactly like
	// every admin/eviction action-path client, no separate wiring needed
	// (P24, .local/specs/node-agent-tls.md section 6).
	resp, err := r.client.Do(req)
	if err != nil {
		// P24: distinguish a fingerprint mismatch from any other dial/network
		// failure so the dashboard can surface it as its own status instead
		// of generic "unreachable" (spec section 6).
		mismatch := errors.Is(err, ErrTLSFingerprintMismatch)
		for _, n := range members {
			r.setAgentTLSMismatch(n, mismatch)
			r.agentUnreachable(n)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}

	var t marboragent.Telemetry
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}

	for _, n := range members {
		r.setAgentTLSMismatch(n, false)
		r.applyAgentTelemetry(n, t)
		r.agentReachable(n.Name)
	}
}

// setAgentTLSMismatch updates n's AgentTLSMismatch flag under its own lock -
// a one-line helper purely so every pollAgentHost branch above can set it
// consistently without repeating the lock/unlock pair five times.
func (r *Router) setAgentTLSMismatch(n *NodeState, mismatch bool) {
	n.mu.Lock()
	n.AgentTLSMismatch = mismatch
	n.mu.Unlock()
}

// setAgentStale updates n's AgentStale flag under its own lock - same
// one-line-helper pattern as setAgentTLSMismatch. True only on the
// crossed-threshold path of agentUnreachable ("configured but went dark");
// false on a successful poll and in the no-agent-configured branch, so the
// flag always means exactly "enrolled agent stopped answering".
func (r *Router) setAgentStale(n *NodeState, stale bool) {
	n.mu.Lock()
	n.AgentStale = stale
	n.mu.Unlock()
}

// applyAgentTelemetry writes one member's share of a host-level Telemetry
// snapshot: the shared host/GPU/capability fields identically, and this
// member's own runtime-specific fields matched out of t.Runtimes.
func (r *Router) applyAgentTelemetry(n *NodeState, t marboragent.Telemetry) {
	n.mu.RLock()
	hasGPU := n.VRAMSource == "nvidia"
	pinnedID := n.AgentRuntimeID
	nodePort := portOf(n.URL)
	n.mu.RUnlock()

	entry, matchedID := matchRuntime(t, pinnedID, nodePort)

	n.mu.Lock()
	n.AgentFailures = 0
	n.AgentPresent = true
	n.AgentStale = false
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
	// ahead of what this marbor binary was compiled understanding, in case a
	// future genuinely-breaking protocol bump ever needs to be diagnosed -
	// it never gates or changes any decode/routing behavior itself. Logged
	// once per node, not every poll cycle.
	if t.Agent.ProtocolVersion > marboragent.ProtocolVersion && !n.agentProtocolWarned {
		n.agentProtocolWarned = true
		log.Printf("node %s: agent reports /v1/status protocol_version %d, newer than this marbor understands (%d) - some new agent fields may not be recognized until marbor is upgraded", n.Name, t.Agent.ProtocolVersion, marboragent.ProtocolVersion)
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
		n.AgentGPUs = append([]marboragent.GPUInfo(nil), t.GPU.Devices...)
		n.DriverVersion = t.GPU.DriverVersion
		n.CUDAVersion = t.GPU.CUDAVersion
		// FanPercent/Temperature/PowerDrawW/VRAM* fall back to the primary
		// (device 0) reading for the marbor's own single-value routing/UI
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
			// data from the marbor host's OWN local nvidia-smi (hasGPU) - a
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
	if entry != nil {
		n.AgentRuntime = entry.Name
		n.RuntimeVersion = entry.Version
		n.RuntimeStatus = entry.Status
		n.AgentRuntimeID = matchedID
	} else {
		n.AgentRuntime = ""
		n.RuntimeVersion = ""
		n.RuntimeStatus = ""
		n.AgentRuntimeID = ""
	}
	if t.Control != nil && t.Control.Discovered != nil {
		n.AgentControlDiscoveredDriver = t.Control.Discovered.Driver
		n.AgentControlDiscoveredIdentifier = t.Control.Discovered.Identifier
		n.AgentControlDiscoveredEvidence = append([]string(nil), t.Control.Discovered.Evidence...)
	} else {
		n.AgentControlDiscoveredDriver = ""
		n.AgentControlDiscoveredIdentifier = ""
		n.AgentControlDiscoveredEvidence = nil
	}
	n.mu.Unlock()
}

// matchRuntime picks the *marboragent.RuntimeInfo (from t.Runtimes, or the
// legacy singular t.Runtime for an old agent build) that corresponds to one
// node row, and the RuntimeID it should now be pinned to (unchanged from
// pinnedID when falling back to the legacy field, since that carries no
// ID). Matching order:
//  1. pinnedID already set and still present in t.Runtimes -> stays pinned,
//     stable across a port edit to this node's URL (the whole point of
//     RuntimeID - see runtime_identity.go).
//  2. No pin yet (first contact, or a marbor restart) -> bootstrap by port
//     against this node's own configured URL port.
//  3. t.Runtimes is empty -> fall back to the legacy singular t.Runtime
//     field (an agent build older than this change - R9).
//  4. No match at all -> nil, this node's runtime-specific fields get
//     cleared while the shared host fields (still reachable) stay populated.
func matchRuntime(t marboragent.Telemetry, pinnedID string, nodePort int) (*marboragent.RuntimeInfo, string) {
	if pinnedID != "" {
		for i := range t.Runtimes {
			if t.Runtimes[i].ID == pinnedID {
				return &t.Runtimes[i], pinnedID
			}
		}
	}
	if nodePort > 0 {
		for i := range t.Runtimes {
			if t.Runtimes[i].Port == nodePort {
				return &t.Runtimes[i], t.Runtimes[i].ID
			}
		}
	}
	if len(t.Runtimes) == 0 && t.Runtime != nil {
		return t.Runtime, pinnedID
	}
	return nil, pinnedID
}

// portOf returns rawURL's port as an int, or 0 if it can't be parsed - used
// only as a one-time bootstrap heuristic for matchRuntime, never as
// identity (see NodeState.AgentRuntimeID's field comment).
func portOf(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	p := u.Port()
	if p == "" {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		return 0
	}
	return port
}

// agentUnreachable records a failed agent poll and, once AgentFailures
// crosses r.healthFailureThreshold (the same consecutive-failure hysteresis
// pollNode/markFailure use for the node's own inference-runtime health),
// clears the node's agent-derived telemetry and, if this is a genuine down
// transition (the agent was previously confirmed reachable), fires an
// "agent_down" webhook - the same reuse of the existing node_up/node_down
// webhook mechanism (fireWebhook, router.go). A single dropped poll (one TCP
// blip, one timeout) below that threshold intentionally leaves the last-known
// telemetry in place rather than blanking the dashboard for one cycle. Called
// once per member of a host group that failed to poll - a host-level
// failure clears every node on it, not just one.
func (r *Router) agentUnreachable(n *NodeState) {
	n.mu.Lock()
	n.AgentFailures++
	crossedThreshold := n.AgentFailures >= r.healthFailureThreshold
	nodeURL := n.URL
	n.mu.Unlock()
	if !crossedThreshold {
		return
	}

	clearAgentTelemetry(n)
	// Past the same threshold that clears telemetry, the agent is genuinely
	// dark (not a single dropped poll) - mark it stale so the admin API can
	// alert "configured agent stopped answering" distinctly from "no agent
	// was ever configured for this host" (router.NodeState.AgentStale).
	r.setAgentStale(n, true)
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
func (r *Router) agentReachable(nodeName string) {
	r.mu.RLock()
	var nodeURL string
	for _, n := range r.nodes {
		if n.Name == nodeName {
			n.RLock()
			nodeURL = n.URL
			n.RUnlock()
			break
		}
	}
	r.mu.RUnlock()

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
// value. Called whenever no agent is configured for a node's host, or the
// most recent poll of a configured host's agent failed.
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
	n.AgentRuntimeID = ""
	n.FanPercent = nil
	n.RAMUsedMB = 0
	n.DiskFreeGB = 0
	n.RAMTotalMB = 0
	n.DiskTotalGB = 0
	n.Hostname = ""
	n.UptimeSeconds = 0
	n.BootTime = 0
	// CPUPercent (applyAgentTelemetry's success path, above) is the only
	// writer of NodeState.CPUPercent anywhere in the codebase - reset it
	// here too, or a disabled/unreachable agent's last-reported CPU reading
	// would linger forever with nothing to mark it stale (R1).
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

// buildAgentURL derives a host's /v1/status URL from its bare hostname, the
// configured agent port, and a scheme (derived by the caller from one member
// node's own URL, per R5 - never arithmetic port derivation).
func buildAgentURL(host string, port int, scheme string) (string, error) {
	// host is normally already a bare hostname (NodeState.Host), but a
	// config file or the admin API's optional host field isn't guaranteed
	// to be pre-stripped of a scheme - tolerate that defensively.
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}
	if scheme == "" {
		scheme = "http"
	}
	// u.Hostname() strips the brackets from an IPv6 literal (e.g.
	// "2001:db8::10"), so a bare interpolation of host here would produce
	// an unparseable "http://2001:db8::10:9200/..." (the ":9200" reads as
	// another IPv6 segment). Re-wrap in brackets when host is an IPv6
	// literal before formatting.
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d/v1/status", scheme, host, port), nil
}
