package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// agentTLSTelemetryServer returns a minimal /v1/status TLS server, mirroring
// TestPollAgentTelemetrySuccess's plain-HTTP agentSrv above but over TLS, for
// the TLS-pinned poll-path tests below.
func agentTLSTelemetryServer() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{
				NodeID:          "node-id-1",
				Version:         "v0.17.0",
				ProtocolVersion: 1,
				Platform:        "linux",
				Architecture:    "amd64",
			},
			Capabilities: []string{"status"},
		})
	}))
}

// TestPollAgentHost_TLSMismatchSetsAgentTLSMismatch is the priority
// integration test: a real pollAgentHosts() cycle against a pinned node
// whose agent presents the WRONG certificate must set AgentTLSMismatch true
// - proving the whole chain (dialTLSContext -> ErrTLSFingerprintMismatch ->
// errors.Is in pollAgentHost -> NodeState) actually works end to end, not
// just the unit-level pieces tested separately in tls_dial_test.go.
func TestPollAgentHost_TLSMismatchSetsAgentTLSMismatch(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentTLSTelemetryServer()
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	r.SetMarborAgent("127.0.0.1", true, agentPort, "", "https")

	wrongFP := "SHA256:" + strings.Repeat("0", 64)
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &wrongFP}) {
		t.Fatal("PatchNode returned false")
	}

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	mismatch := r.nodes[0].AgentTLSMismatch
	present := r.nodes[0].AgentPresent
	r.nodes[0].mu.RUnlock()
	if !mismatch {
		t.Error("AgentTLSMismatch = false, want true after a poll against a wrong pinned fingerprint")
	}
	if present {
		t.Error("AgentPresent = true, want false - the poll must not have succeeded")
	}
}

// TestPollAgentHost_SuccessClearsAgentTLSMismatch verifies a subsequent
// successful poll (correct fingerprint) clears a previously-set mismatch
// flag - AgentTLSMismatch must never stick around after the underlying
// cause is resolved.
func TestPollAgentHost_SuccessClearsAgentTLSMismatch(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentTLSTelemetryServer()
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "https://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	r.SetMarborAgent("127.0.0.1", true, agentPort, "", "https")

	wrongFP := "SHA256:" + strings.Repeat("0", 64)
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &wrongFP}) {
		t.Fatal("PatchNode returned false")
	}
	r.pollAgentHosts()
	r.nodes[0].mu.RLock()
	mismatchBefore := r.nodes[0].AgentTLSMismatch
	r.nodes[0].mu.RUnlock()
	if !mismatchBefore {
		t.Fatal("AgentTLSMismatch = false before the fix, want true (test setup problem)")
	}

	correctFP := certFingerprint(t, agentSrv)
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &correctFP}) {
		t.Fatal("PatchNode returned false")
	}
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	mismatchAfter := r.nodes[0].AgentTLSMismatch
	present := r.nodes[0].AgentPresent
	r.nodes[0].mu.RUnlock()
	if mismatchAfter {
		t.Error("AgentTLSMismatch = true after a successful poll with the correct fingerprint, want false")
	}
	if !present {
		t.Error("AgentPresent = false after a successful poll, want true")
	}
}

// TestMixedTLSAndPlaintextFleet_BothPollCorrectly is the mixed-fleet smoke
// coverage for the opt-in, node-by-node migration rule: one node moving to
// https:// must never affect any other node's plaintext polling in the same
// marbor, and vice versa. Two separate NodeState entries on two separate
// hosts, one pinned https, one plain http, polled together in a single pollAgentHosts() cycle.
func TestMixedTLSAndPlaintextFleet_BothPollCorrectly(t *testing.T) {
	tlsPsSrv := nodePSServer()
	defer tlsPsSrv.Close()
	tlsAgentSrv := agentTLSTelemetryServer()
	defer tlsAgentSrv.Close()
	tlsAgentPort := mustPort(t, tlsAgentSrv.URL)

	plainPsSrv := nodePSServer()
	defer plainPsSrv.Close()
	plainAgentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent:        marboragent.Agent{NodeID: "node-id-2", Version: "v0.17.0", ProtocolVersion: 1, Platform: "linux", Architecture: "amd64"},
			Capabilities: []string{"status"},
		})
	}))
	defer plainAgentSrv.Close()
	plainAgentPort := mustPort(t, plainAgentSrv.URL)

	// "127.0.0.1" and "localhost" are two distinct Host map keys that both
	// resolve to loopback - real, independently-dialable addresses (unlike
	// an arbitrary non-resolvable label), needed since pollAgentHosts groups
	// and SetMarborAgent keys strictly by the Host string.
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-tls", URL: "https://127.0.0.1:1", Host: "127.0.0.1"},
		{Name: "gpu-plain", URL: "http://localhost:1", Host: "localhost"},
	}, nil)
	r.SetMarborAgent("127.0.0.1", true, tlsAgentPort, "", "https")
	r.SetMarborAgent("localhost", true, plainAgentPort, "", "http")

	fp := certFingerprint(t, tlsAgentSrv)
	if !r.PatchNode("gpu-tls", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode gpu-tls returned false")
	}

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	tlsPresent, tlsMismatch := r.nodes[0].AgentPresent, r.nodes[0].AgentTLSMismatch
	r.nodes[0].mu.RUnlock()
	if !tlsPresent {
		t.Error("gpu-tls: AgentPresent = false, want true - the pinned https node should poll successfully")
	}
	if tlsMismatch {
		t.Error("gpu-tls: AgentTLSMismatch = true, want false with the correct pin")
	}

	r.nodes[1].mu.RLock()
	plainPresent, plainMismatch := r.nodes[1].AgentPresent, r.nodes[1].AgentTLSMismatch
	r.nodes[1].mu.RUnlock()
	if !plainPresent {
		t.Error("gpu-plain: AgentPresent = false, want true - a plaintext node in the same fleet must be unaffected")
	}
	if plainMismatch {
		t.Error("gpu-plain: AgentTLSMismatch = true, want false - plaintext nodes are never subject to pinning")
	}
	_, _ = tlsPsSrv, plainPsSrv
}

// TestAgentSchemeIndependentOfRuntimeURL is the priority regression test for
// the bug this fix addresses: enabling https:// for a node's Marbor Agent must
// never change, or depend on, the node's runtime URL scheme. The runtime here
// stays a plain http:// server the whole test (as most real runtimes -
// Ollama, vLLM, etc. - are); only the Agent is configured for https://. Both
// must poll successfully at the same time, and node.URL must be byte-for-byte
// unchanged by the Agent's TLS pinning/poll cycle.
func TestAgentSchemeIndependentOfRuntimeURL(t *testing.T) {
	psSrv := nodePSServer() // plain http runtime - never upgraded to TLS
	defer psSrv.Close()
	agentSrv := agentTLSTelemetryServer() // Agent, independently https
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	const runtimeURL = "http://127.0.0.1:1"
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: runtimeURL, Host: "127.0.0.1"},
	}, nil)
	r.SetMarborAgent("127.0.0.1", true, agentPort, "", "https")

	fp := certFingerprint(t, agentSrv)
	if !r.PatchNode("gpu-0", NodePatch{TLSFingerprint: &fp}) {
		t.Fatal("PatchNode returned false")
	}

	r.nodes[0].mu.RLock()
	urlAfterPin := r.nodes[0].URL
	r.nodes[0].mu.RUnlock()
	if urlAfterPin != runtimeURL {
		t.Fatalf("node.URL after pinning the Agent's fingerprint = %q, want unchanged %q - pinning the Agent's certificate must never touch the runtime URL", urlAfterPin, runtimeURL)
	}

	// Poll the runtime health path directly against psSrv (pollNode normally
	// reads node.URL, which here is the placeholder http://127.0.0.1:1 - psSrv
	// is a separate real listener standing in for "the runtime is reachable
	// over plain http", proven independently of the agent poll below).
	pollResp, err := http.Get(psSrv.URL + "/api/ps")
	if err != nil {
		t.Fatalf("runtime (plain http) unreachable: %v", err)
	}
	pollResp.Body.Close()

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	agentPresent, agentMismatch, urlAfterPoll := r.nodes[0].AgentPresent, r.nodes[0].AgentTLSMismatch, r.nodes[0].URL
	r.nodes[0].mu.RUnlock()
	if !agentPresent {
		t.Error("AgentPresent = false, want true - the https:// Agent poll should have succeeded")
	}
	if agentMismatch {
		t.Error("AgentTLSMismatch = true, want false with the correct pin")
	}
	if urlAfterPoll != runtimeURL {
		t.Errorf("node.URL after an Agent poll cycle = %q, want unchanged %q - polling the Agent must never mutate the runtime URL", urlAfterPoll, runtimeURL)
	}
}

// TestPollAgentHost_GenericFailureDoesNotSetTLSMismatch verifies a plain
// unreachable-node failure (nothing listening on the agent port, and the
// node isn't even https://) never gets misreported as a TLS mismatch - the
// two must stay distinguishable in both directions.
func TestPollAgentHost_GenericFailureDoesNotSetTLSMismatch(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Host: "127.0.0.1"},
	}, nil)
	// Port 1 is not a real listening agent - SetMarborAgent enables polling
	// against it, which will fail as an ordinary connection error.
	r.SetMarborAgent("127.0.0.1", true, 1, "", "http")

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	mismatch := r.nodes[0].AgentTLSMismatch
	failures := r.nodes[0].AgentFailures
	r.nodes[0].mu.RUnlock()
	if mismatch {
		t.Error("AgentTLSMismatch = true for a generic connection failure, want false")
	}
	if failures == 0 {
		t.Error("AgentFailures = 0, want the failed poll to have been counted")
	}
	_ = psSrv
}
