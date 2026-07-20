package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newModelsRequest(t *testing.T, s *Server, node string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/nodes/"+node+"/models", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	return req
}

// TestHandleNodeModels_DispatchesToAgentWhenCapable verifies the mesh
// forwards to the node's Node Agent (GET /v1/models, capability
// "models.list") and translates its snake_case wire response into this
// API's camelCase shape.
func TestHandleNodeModels_DispatchesToAgentWhenCapable(t *testing.T) {
	var gotAuth, gotMethod string
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3.2:latest","size_bytes":2223334444,"source":"ollama-tags"}]}`))
	}))
	defer mockAgent.Close()

	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "agent-secret-token")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentCapabilities = []string{"status", "models.pull", "models.list"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newModelsRequest(t, s, "gpu-0"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected agent request method GET, got %q", gotMethod)
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}

	var resp struct {
		Models []nodeModelEntry `json:"models"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Models))
	}
	if resp.Models[0].Name != "llama3.2:latest" || resp.Models[0].SizeBytes != 2223334444 || resp.Models[0].Source != "ollama-tags" {
		t.Errorf("unexpected model: %+v", resp.Models[0])
	}
}

// TestHandleNodeModels_NoAgentCapabilityReturns501 verifies a node without
// the agent capability gets a clear, honest error - never a fabricated
// empty-but-successful list (R1). There is no direct-HTTP fallback for
// listing local models, unlike pull.
func TestHandleNodeModels_NoAgentCapabilityReturns501(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newModelsRequest(t, s, "gpu-0"))

	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Result().StatusCode)
	}
}

func TestHandleNodeModels_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newModelsRequest(t, s, "does-not-exist"))

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}
