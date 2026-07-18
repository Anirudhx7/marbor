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

// TestAgentDownUpWebhookFiresOnTransition verifies an operator gets an
// agent_down webhook when a configured Node Agent stops responding, and an
// agent_up webhook when it recovers - independent of the node's own
// inference-runtime health webhooks (node_up/node_down), which cover a
// different failure (Ollama itself, not the telemetry sidecar). The very
// first poll of a freshly-enabled agent must not itself fire agent_up (no
// prior "down" state to recover from), matching pollNode's node_up gate.
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
		json.NewEncoder(w).Encode(nodeagent.Telemetry{SchemaVersion: 1, AgentVersion: "v0.16.0"})
	}))
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)
	r.SetWebhookConfig(config.WebhookConfig{Enabled: true, URL: whSrv.URL})
	r.SetNodeAgent("gpu-0", true, agentPort, "tok")

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
	r.pollNode(r.nodes[0])
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	gotFirst := len(received)
	mu.Unlock()
	if gotFirst != 0 {
		t.Fatalf("received %d webhook(s) after first-ever successful poll, want 0 (not a recovery)", gotFirst)
	}

	// Agent goes down.
	agentMu.Lock()
	agentUp = false
	agentMu.Unlock()
	r.pollNode(r.nodes[0])
	waitForCount(1)

	// Agent recovers.
	agentMu.Lock()
	agentUp = true
	agentMu.Unlock()
	r.pollNode(r.nodes[0])
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

// TestPollAgentTelemetryStillPolledWhenAPIPSFails is a regression test for a
// bug where pollNode returned early on a /api/ps probe failure before ever
// reaching pollAgentTelemetry - a node whose Ollama process crashed while its
// Node Agent stayed up would show frozen, stale agent telemetry forever
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
		json.NewEncoder(w).Encode(nodeagent.Telemetry{
			SchemaVersion: 1,
			AgentVersion:  "v0.16.0",
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
