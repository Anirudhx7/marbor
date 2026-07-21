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

func newAdminModelsRequest(t *testing.T, s *Server) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	return req
}

type modelsListResponse struct {
	Models []struct {
		Name      string `json:"name"`
		WarmCount int    `json:"warm_count"`
		Nodes     []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
		} `json:"nodes"`
	} `json:"models"`
	TotalModels int `json:"total_models"`
}

// TestHandleModels_OllamaOnlyFleetUnchanged verifies P36's new Node Agent
// models.list merge is a strict no-op for a fleet with no agent capability -
// handleModels must fall back to exactly the same two sources (LoadedModels +
// FetchModelTags) it always has.
func TestHandleModels_OllamaOnlyFleetUnchanged(t *testing.T) {
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.LoadedModels = []router.ModelInfo{{Name: "llama3.1:8b", SizeVRAM: 123}}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newAdminModelsRequest(t, s))

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var resp modelsListResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TotalModels != 1 {
		t.Fatalf("expected 1 model, got %d: %+v", resp.TotalModels, resp.Models)
	}
	if resp.Models[0].Name != "llama3.1:8b" || resp.Models[0].WarmCount != 1 || len(resp.Models[0].Nodes) != 1 {
		t.Errorf("unexpected model entry: %+v", resp.Models[0])
	}
}

// TestHandleModels_MergesAgentIdleModels verifies handleModels' new third
// source: a node with agentPresent && agentCapabilities includes
// "models.list" contributes idle (downloaded, not loaded) models as rows,
// without double-counting a model already reported warm via LoadedModels.
func TestHandleModels_MergesAgentIdleModels(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[
			{"name":"llama3.1:8b","size_bytes":111,"source":"ollama-tags"},
			{"name":"phi3:mini","size_bytes":222,"source":"hf-cache"}
		]}`))
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
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	r.SetNodeAgent("gpu-0", true, agentPort, "agent-secret-token")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.LoadedModels = []router.ModelInfo{{Name: "llama3.1:8b", SizeVRAM: 123}}
			n.AgentCapabilities = []string{"status", "models.pull", "models.list"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newAdminModelsRequest(t, s))

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var resp modelsListResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TotalModels != 2 {
		t.Fatalf("expected 2 models (1 warm + 1 idle from agent), got %d: %+v", resp.TotalModels, resp.Models)
	}
	byName := map[string]int{}
	for _, m := range resp.Models {
		byName[m.Name] = m.WarmCount
		if m.Name == "llama3.1:8b" && len(m.Nodes) != 1 {
			t.Errorf("llama3.1:8b should not be double-counted on gpu-0, got nodes: %+v", m.Nodes)
		}
	}
	if wc, ok := byName["llama3.1:8b"]; !ok || wc != 1 {
		t.Errorf("expected llama3.1:8b warm_count=1, got %v (present=%v)", wc, ok)
	}
	if wc, ok := byName["phi3:mini"]; !ok || wc != 0 {
		t.Errorf("expected phi3:mini idle (warm_count=0) from agent merge, got %v (present=%v)", wc, ok)
	}
}
