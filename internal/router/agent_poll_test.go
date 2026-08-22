package router

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func TestMarborAgentConfig_TokenNeverMarshaled(t *testing.T) {
	cfg := MarborAgentConfig{Enabled: true, Port: 9200, Token: "secret-value"}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), "secret-value") {
		t.Fatalf("MarborAgentConfig must never marshal Token (P68 - closes config-dump leak path), got %s", b)
	}
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return p
}

// nodePSServer returns a minimal /api/ps server so pollNode's normal health
// poll succeeds (a prerequisite for reaching the agent-poll step it now
// triggers).
func nodePSServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []map[string]interface{}{}})
	}))
}

// TestPollAgentTelemetryNoAgentConfigured verifies the R1 contract: a node
// with no Marbor Agent configured must report zero-value/"unknown" telemetry
// (AgentPresent=false, every agent-derived field cleared), never fabricated
// data, whether or not it was ever enabled in the past.
func TestPollAgentTelemetryNoAgentConfigured(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)

	r.pollNode(r.nodes[0])
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].AgentPresent {
		t.Error("AgentPresent = true, want false when no agent is configured")
	}
	if r.nodes[0].FanPercent != nil {
		t.Errorf("FanPercent = %v, want nil", r.nodes[0].FanPercent)
	}
	if r.nodes[0].RAMUsedMB != 0 {
		t.Errorf("RAMUsedMB = %d, want 0", r.nodes[0].RAMUsedMB)
	}
	if r.nodes[0].DiskFreeGB != 0 {
		t.Errorf("DiskFreeGB = %v, want 0", r.nodes[0].DiskFreeGB)
	}
	if r.nodes[0].AgentVersion != "" {
		t.Errorf("AgentVersion = %q, want empty", r.nodes[0].AgentVersion)
	}
}

// TestPollAgentTelemetrySuccess verifies a configured, reachable agent's
// telemetry is applied to the node, and that the marbor presents the
// configured bearer token when polling it.
func TestPollAgentTelemetrySuccess(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	const token = "sk-agent-test-token"
	fan := 61.0
	temp := 70.0
	power := 250.0
	cpu := 40.0
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tel := marboragent.Telemetry{
			Agent: marboragent.Agent{
				NodeID:          "node-id-1",
				Version:         "v0.16.0",
				ProtocolVersion: 1,
				Platform:        "linux",
				Architecture:    "amd64",
			},
			Capabilities: []string{"status"},
			GPU: &marboragent.GPUBlock{
				Count:  1,
				Vendor: "nvidia",
				Devices: []marboragent.GPUInfo{
					{Index: 0, Vendor: "nvidia", TemperatureC: &temp, FanPercent: &fan, PowerWatts: &power, VRAMUsedMB: 8000, VRAMTotalMB: 16000},
				},
			},
			Host: &marboragent.HostTelemetry{
				CPUPercent: &cpu,
				RAMUsedMB:  4000,
				DiskFreeGB: 100,
			},
			Runtime: &marboragent.RuntimeInfo{Name: "ollama"},
		}
		json.NewEncoder(w).Encode(tel)
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, token, "http")

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true after a successful agent poll")
	}
	if r.nodes[0].AgentNodeID != "node-id-1" {
		t.Errorf("AgentNodeID = %q, want node-id-1", r.nodes[0].AgentNodeID)
	}
	if r.nodes[0].AgentVersion != "v0.16.0" {
		t.Errorf("AgentVersion = %q, want v0.16.0", r.nodes[0].AgentVersion)
	}
	if r.nodes[0].FanPercent == nil || *r.nodes[0].FanPercent != 61 {
		t.Errorf("FanPercent = %v, want 61", r.nodes[0].FanPercent)
	}
	if r.nodes[0].RAMUsedMB != 4000 {
		t.Errorf("RAMUsedMB = %d, want 4000", r.nodes[0].RAMUsedMB)
	}
	if r.nodes[0].DiskFreeGB != 100 {
		t.Errorf("DiskFreeGB = %v, want 100", r.nodes[0].DiskFreeGB)
	}
	// This node isn't a local nvidia-smi node (hasGPU=false, remote-style
	// psSrv), so agent-reported GPU figures should have been applied.
	if r.nodes[0].Temperature == nil || *r.nodes[0].Temperature != 70 {
		t.Errorf("Temperature = %v, want 70 (from agent)", r.nodes[0].Temperature)
	}
	if r.nodes[0].VRAMSource != "agent" {
		t.Errorf("VRAMSource = %q, want agent", r.nodes[0].VRAMSource)
	}
	if r.nodes[0].VRAMTotalMB != 16000 || r.nodes[0].VRAMUsedMB != 8000 {
		t.Errorf("VRAM = %d/%d, want 8000/16000", r.nodes[0].VRAMUsedMB, r.nodes[0].VRAMTotalMB)
	}
	if len(r.nodes[0].AgentCapabilities) != 1 || r.nodes[0].AgentCapabilities[0] != "status" {
		t.Errorf("AgentCapabilities = %v, want [status]", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].AgentPlatform != "linux" || r.nodes[0].AgentArchitecture != "amd64" {
		t.Errorf("AgentPlatform/AgentArchitecture = %q/%q, want linux/amd64", r.nodes[0].AgentPlatform, r.nodes[0].AgentArchitecture)
	}
	if r.nodes[0].AgentGPUVendor != "nvidia" {
		t.Errorf("AgentGPUVendor = %q, want nvidia", r.nodes[0].AgentGPUVendor)
	}
	if r.nodes[0].AgentGPUCount != 1 {
		t.Errorf("AgentGPUCount = %d, want 1", r.nodes[0].AgentGPUCount)
	}
	if len(r.nodes[0].AgentGPUs) != 1 {
		t.Errorf("AgentGPUs = %v, want 1 device", r.nodes[0].AgentGPUs)
	}
	if r.nodes[0].AgentRuntime != "ollama" {
		t.Errorf("AgentRuntime = %q, want ollama", r.nodes[0].AgentRuntime)
	}
}

// TestPollAgentTelemetryFillsGPUModelWhenUnset verifies a node added without
// a GPU model label (empty, or the UI's literal "Unknown GPU" placeholder -
// see GPUNodes.tsx) gets it auto-filled from the agent's reported card
// product name, but a name an operator explicitly set is left untouched.
func TestPollAgentTelemetryFillsGPUModelWhenUnset(t *testing.T) {
	psSrv0 := nodePSServer()
	defer psSrv0.Close()
	psSrv1 := nodePSServer()
	defer psSrv1.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			GPU: &marboragent.GPUBlock{
				Count:  1,
				Vendor: "nvidia",
				Devices: []marboragent.GPUInfo{
					{Index: 0, Vendor: "nvidia", Model: "NVIDIA GeForce RTX 4090"},
				},
			},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv0.URL, GPUModel: "Unknown GPU"},
		{Name: "gpu-1", URL: psSrv1.URL, GPUModel: "My Custom Label"},
	}, nil)
	// Both nodes' URLs are httptest servers, which bind to 127.0.0.1 - same
	// default Host, so they legitimately share one agent config/poll (the
	// whole point of this change: one physical machine, multiple runtimes).
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "", "http")

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	got0 := r.nodes[0].GPUModel
	r.nodes[0].mu.RUnlock()
	if got0 != "NVIDIA GeForce RTX 4090" {
		t.Errorf("GPUModel = %q, want agent-reported NVIDIA GeForce RTX 4090 to replace the Unknown GPU placeholder", got0)
	}

	r.nodes[1].mu.RLock()
	got1 := r.nodes[1].GPUModel
	r.nodes[1].mu.RUnlock()
	if got1 != "My Custom Label" {
		t.Errorf("GPUModel = %q, want operator-set label preserved, not overwritten by the agent", got1)
	}
}

// TestPollAgentTelemetryWrongTokenClearsFields verifies that a configured
// agent rejecting the marbor's token (401) is treated the same as an
// unreachable agent: fields are cleared, not left at a stale prior value.
func TestPollAgentTelemetryWrongTokenClearsFields(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "wrong-token-mismatch", "http")

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].AgentPresent {
		t.Error("AgentPresent = true, want false after a 401 from the agent")
	}
}

// TestPollAgentTelemetryDisabledClearsStaleFields verifies that disabling an
// agent (SetMarborAgent with enabled=false) clears out previously-reported
// fields on the next poll rather than leaving them stuck at their last
// value (R1: never show data that's no longer being measured).
func TestPollAgentTelemetryDisabledClearsStaleFields(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	fan := 61.0
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent:        marboragent.Agent{Version: "v0.16.0", ProtocolVersion: 1, Platform: "linux", Architecture: "amd64"},
			Capabilities: []string{"status"},
			GPU:          &marboragent.GPUBlock{Count: 1, Vendor: "nvidia", Devices: []marboragent.GPUInfo{{Index: 0, FanPercent: &fan}}},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	present := r.nodes[0].AgentPresent
	r.nodes[0].mu.RUnlock()
	if !present {
		t.Fatal("precondition failed: agent poll did not succeed")
	}

	// Now disable and poll again.
	r.SetMarborAgent(r.nodes[0].Host, false, 0, "", "http")
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].AgentPresent {
		t.Error("AgentPresent = true after disabling the agent, want false")
	}
	if r.nodes[0].FanPercent != nil {
		t.Errorf("FanPercent = %v after disabling, want nil (cleared)", r.nodes[0].FanPercent)
	}
	if r.nodes[0].AgentCapabilities != nil {
		t.Errorf("AgentCapabilities = %v after disabling, want nil (cleared)", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].AgentPlatform != "" || r.nodes[0].AgentGPUVendor != "" {
		t.Errorf("AgentPlatform/AgentGPUVendor = %q/%q after disabling, want cleared", r.nodes[0].AgentPlatform, r.nodes[0].AgentGPUVendor)
	}
}

// TestPollAgentTelemetryTransientGPUErrorClearsStaleReadings is a regression
// test: when a GPU backend is selected (vendor known) but a cycle's
// Collect() failed - reported as gpu.vendor set with an empty devices array,
// not a nil gpu block - the marbor must NOT keep showing the previous poll's
// VRAM/temperature/power as current. AgentPresent stays true (the agent
// itself is reachable) but every per-device reading must clear, the same way
// a fully-absent gpu block already does (R1: a stale reading must never
// survive under a "this is live" flag just because the vendor fact didn't
// change).
func TestPollAgentTelemetryTransientGPUErrorClearsStaleReadings(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	temp := 70.0
	power := 250.0
	// First poll: a full, healthy reading with real VRAM/temperature/power.
	// Second poll: same vendor known, but zero devices (the transient
	// Collect() failure shape) - everything device-derived must clear.
	var callCount int
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(marboragent.Telemetry{
				Agent: marboragent.Agent{Version: "v0.17.0", ProtocolVersion: 1},
				GPU: &marboragent.GPUBlock{
					Count: 1, Vendor: "nvidia",
					Devices: []marboragent.GPUInfo{{Index: 0, TemperatureC: &temp, PowerWatts: &power, VRAMUsedMB: 8000, VRAMTotalMB: 16000}},
				},
			})
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v0.17.0", ProtocolVersion: 1},
			GPU:   &marboragent.GPUBlock{Count: 0, Vendor: "nvidia", Devices: []marboragent.GPUInfo{}},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	r.pollAgentHosts()
	r.nodes[0].mu.RLock()
	preTemp, preVRAM := r.nodes[0].Temperature, r.nodes[0].VRAMTotalMB
	r.nodes[0].mu.RUnlock()
	if preTemp == nil || *preTemp != 70 || preVRAM != 16000 {
		t.Fatalf("precondition failed: first poll should have populated Temperature=70/VRAMTotalMB=16000, got %v/%d", preTemp, preVRAM)
	}

	r.pollAgentHosts()
	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true - the agent is still reachable, only its GPU reading failed this cycle")
	}
	if r.nodes[0].AgentGPUVendor != "nvidia" {
		t.Errorf("AgentGPUVendor = %q, want nvidia (still known even when Collect() failed)", r.nodes[0].AgentGPUVendor)
	}
	if r.nodes[0].Temperature != nil {
		t.Errorf("Temperature = %v after a zero-device poll, want nil (cleared, not stale)", r.nodes[0].Temperature)
	}
	if r.nodes[0].PowerDrawW != 0 {
		t.Errorf("PowerDrawW = %v after a zero-device poll, want 0 (cleared, not stale)", r.nodes[0].PowerDrawW)
	}
	if r.nodes[0].VRAMTotalMB != 0 || r.nodes[0].VRAMUsedMB != 0 {
		t.Errorf("VRAM = %d/%d after a zero-device poll, want 0/0 (cleared, not stale)", r.nodes[0].VRAMUsedMB, r.nodes[0].VRAMTotalMB)
	}
	if r.nodes[0].VRAMSource == "agent" {
		t.Errorf("VRAMSource = %q, want reset off 'agent' once the agent's own reading failed", r.nodes[0].VRAMSource)
	}
}

// TestPollAgentTelemetryForwardCompatUnknownFieldsIgnored proves the rolling-
// upgrade contract for a NEWER agent talking to an OLDER marbor: extra JSON
// fields this marbor binary's marboragent.Telemetry struct doesn't define must
// be silently ignored (Go's default json.Decoder behavior - no
// DisallowUnknownFields anywhere in this path), never cause the poll to be
// treated as a failure. Every currently-known field must still populate
// correctly alongside the unrecognized ones.
func TestPollAgentTelemetryForwardCompatUnknownFieldsIgnored(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hand-built JSON (not marboragent.Telemetry) so it can include fields
		// that don't exist in this marbor binary's struct yet - simulating a
		// future agent build's response.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"agent": {
				"version": "v99.0.0",
				"protocol_version": 1,
				"platform": "linux",
				"architecture": "amd64",
				"a_future_agent_field": "surprise"
			},
			"capabilities": ["status", "diagnostics", "actions"],
			"a_field_this_mesh_has_never_heard_of": {"nested": ["stuff", 1, true]},
			"another_unknown_field": 42,
			"gpu": {"vendor": "nvidia", "devices": [{"index": 0, "vram_used_mb": 8000, "vram_total_mb": 16000}]},
			"health": {"runtime_reachable": false},
			"last_updated": "2026-07-17T00:00:00Z"
		}`))
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true - unknown fields must not fail the poll")
	}
	if r.nodes[0].AgentVersion != "v99.0.0" {
		t.Errorf("AgentVersion = %q, want v99.0.0", r.nodes[0].AgentVersion)
	}
	// capabilities lists a future feature ("diagnostics"/"actions") this
	// marbor doesn't implement anything for yet - it must still be stored
	// verbatim, not truncated/rejected, so a future marbor build that DOES
	// understand those capabilities doesn't need the agent to re-report.
	if len(r.nodes[0].AgentCapabilities) != 3 {
		t.Errorf("AgentCapabilities = %v, want 3 entries preserved as-is", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].VRAMTotalMB != 16000 {
		t.Errorf("VRAMTotalMB = %d, want 16000 (known fields must still decode alongside unknown ones)", r.nodes[0].VRAMTotalMB)
	}
}

// TestPollAgentTelemetryBackwardCompatMissingFieldsAreUnknown proves the
// rolling-upgrade contract for an OLDER agent talking to a NEWER marbor: a
// response missing fields this marbor's Telemetry struct now defines
// (capabilities/platform/architecture/gpu vendor/runtime) must decode to
// their zero value and be treated as "not reported" - never crash the poll,
// never be displayed as a real (empty-string/nil) measurement instead of
// "unknown."
func TestPollAgentTelemetryBackwardCompatMissingFieldsAreUnknown(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simulates a very early agent response shape, before
		// capabilities/platform/architecture/gpu/runtime existed.
		w.Write([]byte(`{"agent": {"version": "v0.15.0", "protocol_version": 1}, "last_updated": "2026-07-17T00:00:00Z"}`))
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true - a missing new field must not fail the poll")
	}
	if r.nodes[0].AgentVersion != "v0.15.0" {
		t.Errorf("AgentVersion = %q, want v0.15.0", r.nodes[0].AgentVersion)
	}
	if r.nodes[0].AgentCapabilities != nil {
		t.Errorf("AgentCapabilities = %v, want nil (not reported by this old agent)", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].AgentPlatform != "" || r.nodes[0].AgentGPUVendor != "" {
		t.Errorf("AgentPlatform/AgentGPUVendor = %q/%q, want empty (not reported by this old agent)", r.nodes[0].AgentPlatform, r.nodes[0].AgentGPUVendor)
	}
}

// TestPollAgentTelemetryNewerProtocolVersionLoggedOnce verifies the operator-
// visibility log fires exactly once per node (not every poll cycle) when an
// agent reports a protocol_version ahead of what this marbor binary
// understands - purely informational, per agent_poll.go's comment: decoding
// above already works regardless, this never gates behavior.
func TestPollAgentTelemetryNewerProtocolVersionLoggedOnce(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v99.0.0", ProtocolVersion: marboragent.ProtocolVersion + 1},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	var logBuf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOutput)

	r.pollAgentHosts()
	r.pollAgentHosts()
	r.pollAgentHosts()

	out := logBuf.String()
	count := strings.Count(out, "newer than this marbor understands")
	if count != 1 {
		t.Errorf("protocol-mismatch log appeared %d times across 3 polls, want exactly 1 (latched, not repeated)\nlog output:\n%s", count, out)
	}
}

// TestAgentDownUpWebhookFiresOnTransition verifies an operator gets an
// agent_down webhook when a configured Marbor Agent stops responding, and an
// agent_up webhook when it recovers - independent of the node's own
// inference-runtime health webhooks (node_up/node_down), which cover a
// different failure (Ollama itself, not the agent). The very first poll of a
// freshly-enabled agent must not itself fire agent_up (no prior "down" state
// to recover from), matching pollNode's node_up gate.
func TestAgentDownUpWebhookFiresOnTransition(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]string
	)
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer whSrv.Close()

	psSrv := nodePSServer()
	defer psSrv.Close()

	agentUp := true
	var agentMu sync.Mutex
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentMu.Lock()
		up := agentUp
		agentMu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{Agent: marboragent.Agent{Version: "v0.16.0", ProtocolVersion: 1}})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetWebhookConfig(config.WebhookConfig{Enabled: true, URL: whSrv.URL})
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	waitForCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(received)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d webhook call(s)", n)
	}

	// First successful poll: agent comes up for the first time ever - no
	// agent_up webhook expected (nothing to "recover" from).
	r.pollAgentHosts()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	gotFirst := len(received)
	mu.Unlock()
	if gotFirst != 0 {
		t.Fatalf("received %d webhook(s) after first-ever successful poll, want 0 (not a recovery)", gotFirst)
	}

	// Agent goes down. agent_down only fires once AgentFailures crosses
	// r.healthFailureThreshold (default 3) - a single dropped poll must not
	// blank telemetry or fire the webhook (see agentUnreachable's hysteresis).
	agentMu.Lock()
	agentUp = false
	agentMu.Unlock()
	r.pollAgentHosts()
	r.pollAgentHosts()
	r.pollAgentHosts()
	waitForCount(1)

	// Agent recovers.
	agentMu.Lock()
	agentUp = true
	agentMu.Unlock()
	r.pollAgentHosts()
	waitForCount(2)

	mu.Lock()
	defer mu.Unlock()
	if received[0]["event"] != "agent_down" {
		t.Errorf("first event = %q, want agent_down", received[0]["event"])
	}
	if received[1]["event"] != "agent_up" {
		t.Errorf("second event = %q, want agent_up", received[1]["event"])
	}
	for _, p := range received {
		if p["node"] != "gpu-0" {
			t.Errorf("node = %q, want gpu-0", p["node"])
		}
	}
}

// TestPollAgentTelemetry_ContinuityHysteresisKeepsTelemetryBelowThreshold
// guards the continuity-bug class (LESSONS.md L22 / commit d6012d8):
// pollAgentTelemetry used to clear ALL agent-derived telemetry on the very
// first failed poll. This verifies telemetry survives (is not cleared)
// through healthFailureThreshold-1 consecutive failures - a single dropped
// TCP connection or timeout must not blank the dashboard for that node - and
// only clears once AgentFailures actually crosses the threshold.
func TestPollAgentTelemetry_ContinuityHysteresisKeepsTelemetryBelowThreshold(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	fan := 61.0
	up := true
	var upMu sync.Mutex
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upMu.Lock()
		ok := up
		upMu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v0.17.0", ProtocolVersion: 1},
			GPU:   &marboragent.GPUBlock{Count: 1, Devices: []marboragent.GPUInfo{{Index: 0, FanPercent: &fan}}},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	// Establish a healthy baseline.
	r.pollAgentHosts()
	r.nodes[0].mu.RLock()
	if !r.nodes[0].AgentPresent {
		r.nodes[0].mu.RUnlock()
		t.Fatal("precondition failed: agent poll did not succeed")
	}
	r.nodes[0].mu.RUnlock()

	upMu.Lock()
	up = false
	upMu.Unlock()

	// healthFailureThreshold defaults to 3 (router.go) when unset, as here.
	// The first threshold-1 failures must leave telemetry untouched.
	for i := 0; i < r.healthFailureThreshold-1; i++ {
		r.pollAgentHosts()
		r.nodes[0].mu.RLock()
		present, f := r.nodes[0].AgentPresent, r.nodes[0].FanPercent
		r.nodes[0].mu.RUnlock()
		if !present {
			t.Fatalf("AgentPresent went false after only %d failure(s), want it to survive until the threshold (%d)", i+1, r.healthFailureThreshold)
		}
		if f == nil || *f != 61 {
			t.Fatalf("FanPercent cleared after only %d failure(s), want it to survive until the threshold", i+1)
		}
	}

	// The threshold-th failure must clear it.
	r.pollAgentHosts()
	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].AgentPresent {
		t.Error("AgentPresent still true after crossing healthFailureThreshold consecutive failures, want cleared")
	}
	if r.nodes[0].FanPercent != nil {
		t.Errorf("FanPercent = %v after crossing the threshold, want nil (cleared)", r.nodes[0].FanPercent)
	}
}

// TestAgentProtocolWarned_ContinuityWarnsOnceAcrossDownUpCycle guards the
// continuity-bug class (LESSONS.md L22 / commit d6012d8): agentProtocolWarned
// used to reset on every failed poll (inside clearAgentTelemetry), so a
// flapping agent re-logged the "protocol newer than marbor understands" warning
// on every recovery instead of once per node for the process lifetime. This
// verifies the warning does NOT re-fire after a down/up cycle.
func TestAgentProtocolWarned_ContinuityWarnsOnceAcrossDownUpCycle(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	up := true
	var upMu sync.Mutex
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upMu.Lock()
		ok := up
		upMu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v99.0.0", ProtocolVersion: marboragent.ProtocolVersion + 1},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	var logBuf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOutput)

	// First success: warning latches and logs once.
	r.pollAgentHosts()

	// Agent goes down long enough to cross the failure threshold and clear
	// telemetry (agentProtocolWarned must NOT be reset by this).
	upMu.Lock()
	up = false
	upMu.Unlock()
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.pollAgentHosts()
	}

	// Agent recovers, still reporting the same newer protocol version. If the
	// bug were present, agentProtocolWarned would have been reset to false by
	// clearAgentTelemetry and this poll would log the warning a second time.
	upMu.Lock()
	up = true
	upMu.Unlock()
	r.pollAgentHosts()

	out := logBuf.String()
	count := strings.Count(out, "newer than this marbor understands")
	if count != 1 {
		t.Errorf("protocol-mismatch log appeared %d times across a down/up cycle, want exactly 1 (latched for the node's lifetime, not reset by clearing telemetry)\nlog output:\n%s", count, out)
	}
}

// TestPollAgentTelemetryStillPolledWhenAPIPSFails is a regression test for a
// bug where pollNode returned early on a /api/ps probe failure before ever
// reaching pollAgentTelemetry - a node whose Ollama process crashed while its
// Marbor Agent stayed up would show frozen, stale agent telemetry forever
// instead of the agent poll continuing to run (they're independent HTTP
// endpoints; one failing must not freeze the other's last-reported state).
func TestPollAgentTelemetryStillPolledWhenAPIPSFails(t *testing.T) {
	// /api/ps returns 500 - simulates the node's runtime being down/crashed.
	psSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer psSrv.Close()

	fan := 61.0
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v0.16.0", ProtocolVersion: 1},
			GPU:   &marboragent.GPUBlock{Count: 1, Devices: []marboragent.GPUInfo{{Index: 0, FanPercent: &fan}}},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, agentPort, "tok", "http")

	// Agent telemetry is now a fully separate top-level poll pass
	// (pollAgentHosts), not nested inside pollNode at all - the two calls
	// below directly demonstrate that independence: /api/ps failing (via
	// pollNode) has zero effect on whether the agent poll (pollAgentHosts)
	// succeeds.
	r.pollNode(r.nodes[0])
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	// pollNode still marks the node unhealthy via markFailure on the /api/ps
	// failure - that part of the existing behavior is unchanged and expected;
	// this test only asserts the agent poll ALSO ran.
	if !r.nodes[0].AgentPresent {
		t.Error("AgentPresent = false, want true - the agent poll must still run even when /api/ps fails")
	}
	if r.nodes[0].FanPercent == nil || *r.nodes[0].FanPercent != 61 {
		t.Errorf("FanPercent = %v, want 61 - agent telemetry must be collected independently of /api/ps health", r.nodes[0].FanPercent)
	}
}
