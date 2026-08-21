package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/marboragent"
)

// agentTelemetryServer returns a minimal /v1/status server that always
// reports AgentPresent-worthy telemetry, for proving pollAgentHosts can
// still reach a node's Node Agent after UpdateNodeURL runs.
func agentTelemetryServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{
				NodeID:          "node-id-1",
				Version:         "v0.16.0",
				ProtocolVersion: 1,
				Platform:        "linux",
				Architecture:    "amd64",
			},
			Capabilities: []string{"status"},
		})
	}))
}

// TestUpdateNodeURLReDerivesImplicitHost covers Case A (P24 prerequisite fix):
// a node with no explicit config.NodeConfig.Host had its Host derived from
// its URL's hostname. After UpdateNodeURL changes the URL to a different
// host, Host must re-derive from the new hostname - not stay at the old
// value or go empty - so pollAgentHosts keeps finding the Node Agent
// registered under the node's new effective host.
func TestUpdateNodeURLReDerivesImplicitHost(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentTelemetryServer()
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv.URL},
	}, nil)

	if got := r.nodes[0].Host; got != "127.0.0.1" {
		t.Fatalf("initial Host = %q, want 127.0.0.1 (derived from psSrv URL)", got)
	}

	// Change the node's URL to a different hostname that still resolves to
	// loopback, so the agent poll below can actually reach it.
	newURL := strings.Replace(psSrv.URL, "127.0.0.1", "localhost", 1)
	if err := r.UpdateNodeURL("gpu-0", newURL); err != nil {
		t.Fatalf("UpdateNodeURL: %v", err)
	}

	if got := r.nodes[0].Host; got != "localhost" {
		t.Fatalf("Host after UpdateNodeURL = %q, want localhost (re-derived from new URL)", got)
	}

	r.SetNodeAgent(r.nodes[0].Host, true, agentPort, "", "http")
	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true: pollAgentHosts must find the Node Agent under the re-derived Host after a URL edit")
	}
}

// TestUpdateNodeURLPreservesExplicitHost covers Case B: an operator-declared
// config.NodeConfig.Host override must survive a URL edit untouched, even
// though the new URL's hostname differs from both the old URL's hostname and
// the explicit Host.
func TestUpdateNodeURLPreservesExplicitHost(t *testing.T) {
	psSrv1 := nodePSServer()
	defer psSrv1.Close()
	psSrv2 := nodePSServer()
	defer psSrv2.Close()
	agentSrv := agentTelemetryServer()
	defer agentSrv.Close()
	agentPort := mustPort(t, agentSrv.URL)

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: psSrv1.URL, Host: "localhost"},
	}, nil)

	if got := r.nodes[0].Host; got != "localhost" {
		t.Fatalf("initial Host = %q, want localhost (explicit override)", got)
	}

	r.SetNodeAgent("localhost", true, agentPort, "", "http")

	if err := r.UpdateNodeURL("gpu-0", psSrv2.URL); err != nil {
		t.Fatalf("UpdateNodeURL: %v", err)
	}

	if got := r.nodes[0].Host; got != "localhost" {
		t.Fatalf("Host after UpdateNodeURL = %q, want localhost (explicit override must survive URL edit)", got)
	}

	r.pollAgentHosts()

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if !r.nodes[0].AgentPresent {
		t.Fatal("AgentPresent = false, want true: the explicit Host's Node Agent registration must still be found after the URL edit")
	}
}
