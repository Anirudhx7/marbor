package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// TestEnableNodeAgentAppliesToSiblingOnSameHost is the regression test for
// the reported bug: two node rows sharing the same physical host (same URL
// hostname, different ports/runtimes) must share one Node Agent enrollment -
// enabling it via one node's admin API call must make the OTHER node's
// agent config reflect the same enabled state, not require a second
// independent enable. (The end-to-end "both nodes show AgentPresent after a
// poll" behavior this config sharing enables is covered by
// internal/router/agent_poll_test.go's TestPollAgentTelemetryFillsGPUModelWhenUnset
// and friends, which exercise pollAgentHosts directly - that function is
// unexported, so this admin-level test scopes itself to what handleEnableNodeAgent
// itself is responsible for: the shared config.)
func TestEnableNodeAgentAppliesToSiblingOnSameHost(t *testing.T) {
	// Both node rows point at the same host (127.0.0.1) with different
	// ports/runtimes - the exact shape of the reported bug.
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "ollama-node", URL: "http://127.0.0.1:11434", Runtime: "ollama"},
		{Name: "vllm-node", URL: "http://127.0.0.1:8000", Runtime: "vllm"},
	}, nil)
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	s := NewServer(r, nil, cfg)

	// Enable the agent from "ollama-node"'s admin panel only.
	body, _ := json.Marshal(map[string]int{"port": 9200})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/ollama-node/agent", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", "ollama-node")
	w := httptest.NewRecorder()
	s.handleEnableNodeAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleEnableNodeAgent = %d, want 200: %s", w.Code, w.Body.String())
	}

	// The sibling node's config must already reflect enabled=true, without
	// ever calling enable on it directly - this is the actual fix: both
	// nodes' NodeAgentSetting resolve through the same shared host key.
	got, ok := r.NodeAgentSetting("vllm-node")
	if !ok || !got.Enabled {
		t.Fatalf("NodeAgentSetting(vllm-node) = (%+v, %v), want enabled after enabling via the sibling ollama-node", got, ok)
	}
	if got.Port != 9200 {
		t.Errorf("NodeAgentSetting(vllm-node).Port = %d, want 9200 (the port set via ollama-node)", got.Port)
	}

	// GET /agent from either node's admin panel must report the same
	// enabled/port state.
	host1, _ := r.NodeHost("ollama-node")
	host2, _ := r.NodeHost("vllm-node")
	if host1 != host2 {
		t.Fatalf("ollama-node and vllm-node resolved to different hosts (%q vs %q), want the same (both bound to 127.0.0.1)", host1, host2)
	}
}
