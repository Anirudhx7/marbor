package router

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
)

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
// with no Node Agent configured must report zero-value/"unknown" telemetry
// (AgentPresent=false, every agent-derived field cleared), never fabricated
// data, whether or not it was ever enabled in the past.
func TestPollAgentTelemetryNoAgentConfigured(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)

	r.pollNode(r.nodes[0])

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
// telemetry is applied to the node, and that the mesh presents the
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
		if r.URL.Path != "/telemetry" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tel := nodeagent.Telemetry{
			SchemaVersion: 1,
			AgentVersion:  "v0.16.0",
			Capabilities:  []string{"telemetry"},
			Platform:      "linux",
			Architecture:  "amd64",
			GPUVendor:     "nvidia",
			Runtime:       "ollama",
			GPU: &nodeagent.GPUTelemetry{
				TemperatureC: &temp,
				FanPercent:   &fan,
				PowerWatts:   &power,
				VRAMUsedMB:   8000,
				VRAMTotalMB:  16000,
			},
			Host: &nodeagent.HostTelemetry{
				CPUPercent: &cpu,
				RAMUsedMB:  4000,
				DiskFreeGB: 100,
			},
		}
		json.NewEncoder(w).Encode(tel)
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, token)

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true after a successful agent poll")
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
	if len(r.nodes[0].AgentCapabilities) != 1 || r.nodes[0].AgentCapabilities[0] != "telemetry" {
		t.Errorf("AgentCapabilities = %v, want [telemetry]", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].AgentPlatform != "linux" || r.nodes[0].AgentArchitecture != "amd64" {
		t.Errorf("AgentPlatform/AgentArchitecture = %q/%q, want linux/amd64", r.nodes[0].AgentPlatform, r.nodes[0].AgentArchitecture)
	}
	if r.nodes[0].AgentGPUVendor != "nvidia" {
		t.Errorf("AgentGPUVendor = %q, want nvidia", r.nodes[0].AgentGPUVendor)
	}
	if r.nodes[0].AgentRuntime != "ollama" {
		t.Errorf("AgentRuntime = %q, want ollama", r.nodes[0].AgentRuntime)
	}
}

// TestPollAgentTelemetryWrongTokenClearsFields verifies that a configured
// agent rejecting the mesh's token (401) is treated the same as an
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
	r.SetNodeAgent("gpu-0", true, agentPort, "wrong-token-mismatch")

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].AgentPresent {
		t.Error("AgentPresent = true, want false after a 401 from the agent")
	}
}

// TestPollAgentTelemetryDisabledClearsStaleFields verifies that disabling an
// agent (SetNodeAgent with enabled=false) clears out previously-reported
// fields on the next poll rather than leaving them stuck at their last
// value (R1: never show data that's no longer being measured).
func TestPollAgentTelemetryDisabledClearsStaleFields(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	fan := 61.0
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nodeagent.Telemetry{
			SchemaVersion: 1,
			AgentVersion:  "v0.16.0",
			Capabilities:  []string{"telemetry"},
			Platform:      "linux",
			Architecture:  "amd64",
			GPUVendor:     "nvidia",
			GPU:           &nodeagent.GPUTelemetry{FanPercent: &fan},
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "tok")
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	present := r.nodes[0].AgentPresent
	r.nodes[0].mu.RUnlock()
	if !present {
		t.Fatal("precondition failed: agent poll did not succeed")
	}

	// Now disable and poll again.
	r.SetNodeAgent("gpu-0", false, 0, "")
	r.pollNode(r.nodes[0])

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

// TestPollAgentTelemetryForwardCompatUnknownFieldsIgnored proves the rolling-
// upgrade contract for a NEWER agent talking to an OLDER mesh: extra JSON
// fields this mesh binary's nodeagent.Telemetry struct doesn't define must
// be silently ignored (Go's default json.Decoder behavior - no
// DisallowUnknownFields anywhere in this path), never cause the poll to be
// treated as a failure. Every currently-known field must still populate
// correctly alongside the unrecognized ones.
func TestPollAgentTelemetryForwardCompatUnknownFieldsIgnored(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hand-built JSON (not nodeagent.Telemetry) so it can include fields
		// that don't exist in this mesh binary's struct yet - simulating a
		// future agent build's response.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"schema_version": 1,
			"agent_version": "v99.0.0",
			"capabilities": ["telemetry", "diagnostics", "actions"],
			"platform": "linux",
			"architecture": "amd64",
			"gpu_vendor": "nvidia",
			"a_field_this_mesh_has_never_heard_of": {"nested": ["stuff", 1, true]},
			"another_unknown_field": 42,
			"gpu": {"vendor": "nvidia", "vram_used_mb": 8000, "vram_total_mb": 16000},
			"last_updated": "2026-07-17T00:00:00Z"
		}`))
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "tok")
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true - unknown fields must not fail the poll")
	}
	if r.nodes[0].AgentVersion != "v99.0.0" {
		t.Errorf("AgentVersion = %q, want v99.0.0", r.nodes[0].AgentVersion)
	}
	// capabilities lists a future feature ("diagnostics"/"actions") this
	// mesh doesn't implement anything for yet - it must still be stored
	// verbatim, not truncated/rejected, so a future mesh build that DOES
	// understand those capabilities doesn't need the agent to re-report.
	if len(r.nodes[0].AgentCapabilities) != 3 {
		t.Errorf("AgentCapabilities = %v, want 3 entries preserved as-is", r.nodes[0].AgentCapabilities)
	}
	if r.nodes[0].VRAMTotalMB != 16000 {
		t.Errorf("VRAMTotalMB = %d, want 16000 (known fields must still decode alongside unknown ones)", r.nodes[0].VRAMTotalMB)
	}
}

// TestPollAgentTelemetryBackwardCompatMissingFieldsAreUnknown proves the
// rolling-upgrade contract for an OLDER agent talking to a NEWER mesh: a
// response missing fields this mesh's Telemetry struct now defines
// (capabilities/platform/architecture/gpu_vendor/runtime, added after v1
// shipped) must decode to their zero value and be treated as "not reported"
// - never crash the poll, never be displayed as a real (empty-string/nil)
// measurement instead of "unknown."
func TestPollAgentTelemetryBackwardCompatMissingFieldsAreUnknown(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simulates the very first v1 agent response shape, before
		// capabilities/platform/architecture/gpu_vendor/runtime existed.
		w.Write([]byte(`{"schema_version": 1, "agent_version": "v0.15.0", "last_updated": "2026-07-17T00:00:00Z"}`))
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "tok")
	r.pollNode(r.nodes[0])

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

// TestPollAgentTelemetryNewerSchemaVersionLoggedOnce verifies the operator-
// visibility log fires exactly once per node (not every poll cycle) when an
// agent reports a schema_version ahead of what this mesh binary understands
// - purely informational, per agent_poll.go's comment: decoding above
// already works regardless, this never gates behavior.
func TestPollAgentTelemetryNewerSchemaVersionLoggedOnce(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nodeagent.Telemetry{
			SchemaVersion: nodeagent.SchemaVersion + 1,
			AgentVersion:  "v99.0.0",
		})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "tok")

	var logBuf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOutput)

	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])

	out := logBuf.String()
	count := strings.Count(out, "newer than this mesh understands")
	if count != 1 {
		t.Errorf("schema-mismatch log appeared %d times across 3 polls, want exactly 1 (latched, not repeated)\nlog output:\n%s", count, out)
	}
}
