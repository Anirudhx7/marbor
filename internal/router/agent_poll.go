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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// maxAgentStatusBodyBytes caps how much of one agent status response is read.
// A real report is a few KiB to a few hundred KiB; the cap sits far above that
// and exists so a runaway or hostile agent cannot make the router buffer an
// unbounded body. A reply over the cap is not read: the agent still counts as
// reachable, but its telemetry is treated as unknown until a reply fits.
const maxAgentStatusBodyBytes = 8 << 20

// derivePrefixCacheHitRate converts this poll's raw cumulative prefix-cache
// counters into a 0-100% rate against n's previous poll, then updates n's
// baseline for the next call. Returns nil (unknown, never a fabricated 0%)
// when either counter is missing this poll, when there is no prior baseline
// yet (first poll that reports the counters), when the query count did not
// increase (nothing to divide by, or a stalled engine), or when either
// counter went backwards (the engine restarted between polls - a v1-engine
// counter reset to 0 - so the delta would be nonsense).
func derivePrefixCacheHitRate(n *NodeState, queries, hits *float64) *float64 {
	prevQ, prevH := n.prevEnginePrefixCacheQueries, n.prevEnginePrefixCacheHits
	if queries != nil && hits != nil {
		n.prevEnginePrefixCacheQueries = queries
		n.prevEnginePrefixCacheHits = hits
	} else {
		n.prevEnginePrefixCacheQueries = nil
		n.prevEnginePrefixCacheHits = nil
	}
	if queries == nil || hits == nil || prevQ == nil || prevH == nil {
		return nil
	}
	if *queries < *prevQ || *hits < *prevH {
		return nil
	}
	deltaQ := *queries - *prevQ
	deltaH := *hits - *prevH
	if deltaQ <= 0 {
		return nil
	}
	rate := (deltaH / deltaQ) * 100
	if rate < 0 {
		rate = 0
	} else if rate > 100 {
		rate = 100
	}
	return &rate
}

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
		r.setAgentTLSMismatch(n, false)
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
	// A host that no longer has a node row or an enabled agent must stop
	// feeding replica correlation (address identity included), so drop every
	// snapshot whose host is not in this pass.
	r.dropHostEvidenceNotIn(groups)
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

	// Same reliability boundary as pollNode: this runs on its own goroutine, so
	// a panic from one host's poll must not be able to take down the whole
	// process. Treat it as a failed poll for every node on the host.
	defer func() {
		if rec := recover(); rec != nil {
			if r.allowHostLog("panic:" + host) {
				log.Printf("router: recovered panic polling agent on host %q: %s\n%s", host, truncateForLog(fmt.Sprint(rec), maxLogValueBytes), debug.Stack())
			}
			for _, n := range members {
				r.agentUnreachableSafe(n)
			}
		}
	}()

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
		r.logAgentPoll(host, "badurl", "skipped: invalid agent address: %v", err)
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURL, nil)
	if err != nil {
		r.logAgentPoll(host, "badreq", "skipped: cannot build request: %v", err)
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	// r.client's Transport is the Router's single shared, TLS-pinning-aware
	// Transport (see tls_dial.go, HTTPClientForNode) - an https:// agentURL
	// here is verified against this host's pinned fingerprint exactly like
	// every admin/eviction action-path client, no separate wiring needed.
	resp, err := r.client.Do(req)
	if err != nil {
		// Distinguish a fingerprint mismatch from any other dial/network
		// failure so the dashboard can surface it as its own status instead
		// of generic "unreachable".
		mismatch := errors.Is(err, ErrTLSFingerprintMismatch)
		if mismatch {
			r.logAgentPoll(host, "mismatch", "refused: TLS fingerprint mismatch: %v", err)
		} else {
			r.logAgentPoll(host, "dial", "failed: %v", err)
		}
		for _, n := range members {
			r.setAgentTLSMismatch(n, mismatch)
			r.agentUnreachable(n)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.logAgentPoll(host, "status", "failed: agent answered status %d", resp.StatusCode)
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentStatusBodyBytes+1))
	if err != nil {
		if r.allowHostLog("read:" + host) {
			log.Printf("router: reading agent status from host %q failed: %v; treating the agent as unreachable", host, err)
		}
	} else if len(body) > maxAgentStatusBodyBytes {
		if !looksLikeJSONObject(body) {
			// Too large and not even a JSON object (an HTML error page,
			// garbage): no sign of a healthy agent, so unreachable.
			if r.allowHostLog("oversize:" + host) {
				log.Printf("router: agent status response on host %q exceeds %d bytes and is not a JSON object; treating the agent as unreachable", host, maxAgentStatusBodyBytes)
			}
			err = errors.New("oversize agent status reply is not a JSON object")
		} else {
			// The agent answered, so it is reachable; only its telemetry is
			// unknown. Retained host evidence is dropped (it can no longer be
			// refreshed) without resetting the log rate limit; that log key
			// intentionally survives recovery so a flapping agent stays
			// rate-limited. The body is not drained, so this connection is
			// not reused. A down -> oversize transition fires the agent-up
			// webhook through agentReachable: the agent is reachable again.
			if r.allowHostLog("oversize:" + host) {
				log.Printf("router: agent status response exceeds %d bytes on host %q; telemetry is unknown until it shrinks (agent-sourced VRAM reads as unknown and may lower placement score)", maxAgentStatusBodyBytes, host)
			}
			r.dropHostEvidenceOnly(host)
			for _, n := range members {
				r.setAgentTLSMismatch(n, false)
				applyOversizeTelemetry(n)
				r.agentReachable(n.Name)
			}
			return
		}
	}
	var t marboragent.Telemetry
	if err == nil {
		// Default decoding on purpose: fields this binary does not know are
		// ignored so a newer agent keeps working.
		err = json.NewDecoder(bytes.NewReader(body)).Decode(&t)
		if err != nil {
			r.logAgentPoll(host, "decode", "failed: could not decode status reply: %v", err)
		}
	}
	if err != nil {
		for _, n := range members {
			r.setAgentTLSMismatch(n, false)
			r.agentUnreachable(n)
		}
		return
	}

	// Keep the host-wide report for replica correlation. Keyed on host, the
	// same raw key pollAgentHosts grouped by.
	ev := HostEvidence{
		Host:         host,
		Capabilities: t.Capabilities,
		Deployments:  t.Deployments,
		PolledAt:     time.Now(),
	}
	if t.Host != nil {
		ev.Hostname = t.Host.Hostname
		ev.Addrs = t.Host.Addrs
	}
	r.RecordHostEvidence(ev)

	for _, n := range members {
		r.setAgentTLSMismatch(n, false)
		r.applyAgentTelemetryForHost(n, t, len(members))
		r.agentReachable(n.Name)
	}
}

// maxLogValueBytes caps an untrusted or arbitrary value (a panic value, an
// agent-supplied message) embedded in a log line or error string.
const maxLogValueBytes = 256

// truncateForLog cuts s to at most max bytes, marking the cut.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// agentUnreachableSafe clears the TLS-mismatch flag and records a failed poll
// for n, recovering its own panic so one bad node cannot stop the rest of the
// host's members from being marked unreachable.
func (r *Router) agentUnreachableSafe(n *NodeState) {
	defer func() {
		if rec := recover(); rec != nil {
			if r.allowHostLog("panic:" + n.Name) {
				log.Printf("router: recovered panic marking node %q agent unreachable: %s", n.Name, truncateForLog(fmt.Sprint(rec), maxLogValueBytes))
			}
		}
	}()
	r.setAgentTLSMismatch(n, false)
	r.agentUnreachable(n)
}

// logAgentPoll logs one agent poll failure for host, at most once per
// hostLogInterval per kind and host. Polls repeat every interval, so an
// unconditional line would flood the log for an agent that stays down. The
// messages carry the status code or error only; the agent token is never part
// of them.
func (r *Router) logAgentPoll(host, kind, format string, args ...any) {
	if !r.allowHostLog(kind + ":" + host) {
		return
	}
	log.Printf("router: agent poll for host %q "+format, append([]any{host}, args...)...)
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
	r.applyAgentTelemetryForHost(n, t, 1)
}

// applyAgentTelemetryForHost is applyAgentTelemetry for a host that has
// hostMembers nodes sharing it; the count decides whether a deployment report
// with no port can be attributed to this node (see matchDeployment).
func (r *Router) applyAgentTelemetryForHost(n *NodeState, t marboragent.Telemetry, hostMembers int) {
	n.mu.Lock()
	// Deferred so a panic in the body cannot leave the node lock held: the
	// poll's recover path locks the same node again.
	defer n.mu.Unlock()
	r.applyAgentTelemetryLocked(n, t, hostMembers)
}

// applyAgentTelemetryLocked is the body of applyAgentTelemetryForHost; the
// caller holds n.mu.
func (r *Router) applyAgentTelemetryLocked(n *NodeState, t marboragent.Telemetry, hostMembers int) {
	// One critical section for the reads that steer the writes below: a
	// separate read lock first let a concurrent poll change the VRAM source or
	// the runtime pin between the decision and the write.
	if r.applyTelemetryHook != nil {
		r.applyTelemetryHook()
	}
	hasGPU := n.VRAMSource == "nvidia"
	pinnedID := n.AgentRuntimeID
	nodePort := portOfOrDefault(n.URL)
	entry, matchedID := matchRuntime(t, pinnedID, nodePort)

	n.AgentFailures = 0
	n.AgentPresent = true
	n.AgentStale = false
	n.AgentTelemetryUnknown = false
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
		// A host block that omits CPU keeps the earlier value on purpose (see
		// derefOr); a report with no host block at all is handled below.
		n.CPUPercent = derefOr(t.Host.CPUPercent, n.CPUPercent)
		n.RAMUsedMB = t.Host.RAMUsedMB
		n.DiskFreeGB = t.Host.DiskFreeGB
		n.RAMTotalMB = t.Host.RAMTotalMB
		n.DiskTotalGB = t.Host.DiskTotalGB
		n.Hostname = t.Host.Hostname
		n.UptimeSeconds = t.Host.UptimeSeconds
		n.BootTime = t.Host.BootTime
	} else {
		n.CPUPercent = 0
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
				// An omitted power reading is unknown (0), never the last one.
				n.PowerDrawW = derefOr(primary.PowerWatts, 0)
				if primary.VRAMTotalMB > 0 || primary.VRAMUsedMB > 0 {
					n.VRAMTotalMB = primary.VRAMTotalMB
					n.VRAMUsedMB = primary.VRAMUsedMB
					n.VRAMSource = "agent"
				} else if n.VRAMSource == "agent" {
					// The device now reports no VRAM at all: the earlier agent
					// figure is stale, so fall back like every other path.
					fallBackAgentVRAM(n)
				}
			}
		} else {
			// GPU vendor is known (a backend is selected on this node) but this
			// cycle's Collect() failed - a transient nvidia-smi hiccup, not a
			// permanent "no GPU" state. Clear every per-cycle reading derived
			// from a device, the same way clearAgentTelemetry's wasAgentSourced
			// branch does, so a stale VRAM/temperature/power figure from the
			// last good poll never keeps displaying as current just
			// because AgentPresent is still true.
			n.FanPercent = nil
			if !hasGPU {
				n.Temperature = nil
				n.PowerDrawW = 0
				if n.VRAMSource == "agent" {
					fallBackAgentVRAM(n)
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
		// No GPU block: every agent-derived GPU reading is stale, same as the
		// no-devices case above.
		if !hasGPU {
			n.Temperature = nil
			n.PowerDrawW = 0
			if n.VRAMSource == "agent" {
				fallBackAgentVRAM(n)
			}
		}
	}
	if entry != nil {
		n.AgentRuntime = entry.Name
		n.RuntimeVersion = entry.Version
		n.RuntimeStatus = entry.Status
		n.AgentRuntimeID = matchedID
		if entry.Engine != nil {
			n.EngineRunningRequests = entry.Engine.RunningRequests
			n.EngineWaitingRequests = entry.Engine.WaitingRequests
			n.EngineKVCacheUsagePercent = entry.Engine.KVCacheUsagePercent
			n.EnginePrefixCacheHitRatePercent = derivePrefixCacheHitRate(n, entry.Engine.PrefixCacheQueries, entry.Engine.PrefixCacheHits)
		} else {
			// This poll's RuntimeInfo carries no Engine block at all (e.g.
			// runtime down, or a runtime with no native metrics source) -
			// clear rather than hold whatever the last successful poll
			// reported. Poll freshness is the only freshness mechanism for
			// EngineState - no separate stale-value TTL.
			n.EngineRunningRequests = nil
			n.EngineWaitingRequests = nil
			n.EngineKVCacheUsagePercent = nil
			n.EnginePrefixCacheHitRatePercent = nil
			n.prevEnginePrefixCacheQueries = nil
			n.prevEnginePrefixCacheHits = nil
		}
	} else {
		n.AgentRuntime = ""
		n.RuntimeVersion = ""
		n.RuntimeStatus = ""
		n.AgentRuntimeID = ""
		n.EngineRunningRequests = nil
		n.EngineWaitingRequests = nil
		n.EngineKVCacheUsagePercent = nil
		n.EnginePrefixCacheHitRatePercent = nil
		n.prevEnginePrefixCacheQueries = nil
		n.prevEnginePrefixCacheHits = nil
	}
	// Per-runtime deployment auto-discovery (port/ID matched, not Host).
	// One deployment report per runtime instance means two vLLM on same host
	// with different TP widths do not fan the wrong 8 GPUs to both nodes.
	if dep := matchDeploymentForHost(t.Deployments, pinnedID, nodePort, hostMembers); dep != nil {
		n.DetectedRuntime = dep.Runtime
		if dep.Parallelism != nil {
			n.DetectedParallelismType = dep.Parallelism.Type
			n.DetectedParallelismWidth = dep.Parallelism.Width
			n.DetectedPipelineWidth = dep.Parallelism.PipelineWidth
			n.DetectedDataWidth = dep.Parallelism.DataWidth
		} else {
			n.DetectedParallelismType = ""
			n.DetectedParallelismWidth = 0
			n.DetectedPipelineWidth = 0
			n.DetectedDataWidth = 0
		}
		if len(dep.GPUGroup) > 0 {
			n.DetectedGPUGroup = append([]int(nil), dep.GPUGroup...)
		} else {
			n.DetectedGPUGroup = nil
		}
		n.DetectedGPUScope = copyGPUScope(dep.GPUScope)
		n.DetectedSource = dep.Source
		n.DetectedCaps = dep.Caps
	} else {
		// No deployment report for this runtime instance - keep honest unknown,
		// never fabricated. Cleared so a stale TP=8 from a prior poll does not linger
		// when the runtime moves ports or the ps becomes invisible (pid ns).
		n.DetectedParallelismType = ""
		n.DetectedParallelismWidth = 0
		n.DetectedPipelineWidth = 0
		n.DetectedDataWidth = 0
		n.DetectedGPUGroup = nil
		n.DetectedGPUScope = nil
		n.DetectedSource = ""
		n.DetectedCaps = nil
		n.DetectedRuntime = ""
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
	// An agent-reported GPU device VRAM reading is a real, non-Ollama
	// signal for node-level VRAMUsedMB (set above) - attributeSoleModelVRAM
	// (health.go) additionally attributes it to the node's sole loaded
	// model's own SizeVRAM when that model's size is otherwise unknown,
	// same single-model-per-host inference pollNode's local-nvidia-smi path
	// already applies. Called here too (not just from pollNode) because
	// this is the only place a *remote* non-Ollama node's VRAMUsedMB is
	// ever set from a real reading - health.go's own default branch would
	// otherwise overwrite VRAMUsedMB back from the runtime's own
	// (non-Ollama: always-zero) API on its next poll cycle without ever
	// re-deriving this attribution.
	attributeSoleModelVRAM(n)
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
//     field (an agent build older than this change).
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

// matchDeployment picks the DeploymentReport for one node row by the same
// pinnedID/port discipline as matchRuntime. A host with two vLLM on
// :8000 TP=8 and :8001 TP=4 must not fan a single host-level report to both
// nodes blind - each node gets the report keyed by its own port/ID.
func matchDeployment(deployments []marboragent.DeploymentReport, pinnedID string, nodePort int) *marboragent.DeploymentReport {
	return matchDeploymentForHost(deployments, pinnedID, nodePort, 1)
}

// matchDeploymentForHost is matchDeployment for a host with hostMembers nodes
// sharing it. A report with no port can only be attributed when the host has
// exactly one node: with several, "the only report" says nothing about which
// of them it describes.
func matchDeploymentForHost(deployments []marboragent.DeploymentReport, pinnedID string, nodePort, hostMembers int) *marboragent.DeploymentReport {
	if pinnedID != "" {
		for i := range deployments {
			if deployments[i].RuntimeID == pinnedID {
				return &deployments[i]
			}
		}
	}
	if nodePort > 0 {
		for i := range deployments {
			if deployments[i].Port == nodePort {
				return &deployments[i]
			}
		}
	}
	// Fallback: single deployment with unknown port on single-node host
	// -> attribute to sole node. Do NOT fallback when deployment has a
	// known port that does not match this node's port - that would
	// misattribute a vLLM TP=8 on :8000 to an Ollama node on :11434 on the
	// same host (e.g. host with Ollama :11434 + vLLM :8000 but ps only saw
	// vLLM). Port-specific reports must stay port-specific.
	if hostMembers == 1 && len(deployments) == 1 && deployments[0].Port == 0 {
		return &deployments[0]
	}
	return nil
}

// portOf returns rawURL's explicit port as an int, or 0 if there is none or it
// can't be determined.
func portOf(rawURL string) int {
	return parsePort(rawURL, false)
}

// portOfOrDefault is portOf but returns 80/443 for an http/https URL with no
// explicit port - used only as a one-time bootstrap heuristic for matchRuntime
// and matchDeployment, never as identity (see NodeState.AgentRuntimeID's field
// comment).
func portOfOrDefault(rawURL string) int {
	return parsePort(rawURL, true)
}

func parsePort(rawURL string, schemeDefault bool) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	p := u.Port()
	if p == "" {
		if !schemeDefault {
			return 0
		}
		// No explicit port: the scheme's default is the port the node is
		// really on.
		switch u.Scheme {
		case "http":
			return 80
		case "https":
			return 443
		}
		return 0
	}
	port, err := strconv.Atoi(p)
	if err != nil {
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
	nodeHost := n.Host
	n.mu.Unlock()
	if !crossedThreshold {
		return
	}

	// Same threshold that clears telemetry also retires the host's replica
	// evidence; a single dropped poll keeps it.
	r.DropHostEvidence(nodeHost)
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
	wentDown := exists && seen && prev
	// Only a real up -> down transition is recorded. Writing "down" for an
	// agent that was never reached would make its first success look like a
	// recovery and fire a spurious agent_up.
	if wentDown {
		r.prevAgentPresent[nodeName] = false
	}
	r.mu.Unlock()
	if wentDown {
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
// most recent poll of a configured host's agent failed. Temperature and power
// clear only when the VRAM was agent-sourced (the agent was then their source);
// applyOversizeTelemetry clears them whenever VRAM is not local nvidia-smi,
// because it keeps the agent present and has no other source to defer to.
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
	n.DriverVersion = ""
	n.CUDAVersion = ""
	n.AgentRuntime = ""
	n.RuntimeVersion = ""
	n.AgentRuntimeID = ""
	clearLiveReadings(n)
	n.Hostname = ""
	// Clear auto-discovered deployment (honest unknown, not stale).
	n.DetectedParallelismType = ""
	n.DetectedParallelismWidth = 0
	n.DetectedPipelineWidth = 0
	n.DetectedDataWidth = 0
	n.DetectedGPUGroup = nil
	n.DetectedGPUScope = nil
	n.DetectedSource = ""
	n.DetectedCaps = nil
	n.DetectedRuntime = ""
	n.AgentTelemetryUnknown = false
	n.AgentControlDiscoveredDriver = ""
	n.AgentControlDiscoveredIdentifier = ""
	n.AgentControlDiscoveredEvidence = nil
	if wasAgentSourced {
		fallBackAgentVRAM(n)
		n.Temperature = nil
		n.PowerDrawW = 0
	}
}

// looksLikeJSONObject reports whether body, ignoring leading JSON whitespace,
// starts with '{'. It is a cheap shape check for a reply too large to decode.
func looksLikeJSONObject(body []byte) bool {
	t := bytes.TrimLeft(body, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}

// clearLiveReadings forgets the per-poll live readings that both the
// agent-gone and the oversize-reply paths drop together. The caller holds n.mu.
func clearLiveReadings(n *NodeState) {
	n.FanPercent = nil
	n.CPUPercent = 0
	n.AgentGPUs = nil
	n.RuntimeStatus = ""
	n.EngineRunningRequests = nil
	n.EngineWaitingRequests = nil
	n.EngineKVCacheUsagePercent = nil
	n.EnginePrefixCacheHitRatePercent = nil
	n.prevEnginePrefixCacheQueries = nil
	n.prevEnginePrefixCacheHits = nil
	n.DiskFreeGB = 0
	n.DiskTotalGB = 0
	n.RAMUsedMB = 0
	n.RAMTotalMB = 0
	n.UptimeSeconds = 0
	n.BootTime = 0
}

// fallBackAgentVRAM drops an agent-sourced VRAM reading back to whatever the
// node's declared figure would otherwise be (or none), same defaulting
// pollNode's non-local branch uses. The caller holds n.mu and has already
// checked that the VRAM was agent-sourced.
func fallBackAgentVRAM(n *NodeState) {
	if n.VRAMTotalMBConfig > 0 {
		n.VRAMTotalMB = n.VRAMTotalMBConfig
		n.VRAMSource = "declared"
	} else {
		n.VRAMTotalMB = 0
		n.VRAMSource = "none"
	}
	n.VRAMUsedMB = 0
}

// applyOversizeTelemetry records that the agent answered but its status reply
// was too large to read. The agent is reachable, so presence and identity
// stay (AgentGPUCount included: it is static identity, not a live reading)
// and the failure counter resets; every live reading is forgotten because
// none of it can be trusted, and AgentTelemetryUnknown says so explicitly.
// Temperature and power are cleared unless the node reads a local nvidia-smi
// (the agent is their only other source); VRAM is dropped back to its
// declared figure only when it was agent-sourced. For an agent-sourced node
// that leaves VRAM reading as unknown/zero, which may lower its placement
// score until the report shrinks; the node stays routable. Disk and RAM clear
// together so the disk-fit check (pinned by internal/admin's
// TestClassifyDiskFitOversizeClearedShapeIsUnknown) reports "unknown" rather than a fabricated
// "insufficient". Last-known detected deployment shape is kept: it describes
// how a runtime was launched, not its load, and replica detection already
// stops because host evidence is dropped.
func applyOversizeTelemetry(n *NodeState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wasAgentSourced := n.VRAMSource == "agent"
	n.AgentFailures = 0
	n.AgentPresent = true
	n.AgentStale = false
	n.AgentTelemetryUnknown = true
	clearLiveReadings(n)
	if n.VRAMSource != "nvidia" {
		n.Temperature = nil
		n.PowerDrawW = 0
	}
	if wasAgentSourced {
		fallBackAgentVRAM(n)
	}
}

// buildAgentURL derives a host's /v1/status URL from its bare hostname, the
// configured agent port, and a scheme (derived by the caller from one member
// node's own URL, via url.Parse - never arithmetic port derivation).
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
