package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// newModelFitTestServer creates an admin Server backed by a router whose nodes
// point at the provided mock Ollama server URL.
func newModelFitTestServer(ollamaURL string) *Server {
	r := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 60000},
		[]config.NodeConfig{
			{Name: "test-node", URL: ollamaURL, GPUModel: "RTX 4090"},
		},
		nil,
	)
	// Mark node healthy and set a loaded model so vramUsedMBFromPS > 0.
	nodes := r.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].VRAMTotalMB = 8192 // 8 GB via nvidia-smi
	nodes[0].LoadedModels = []router.ModelInfo{{Name: "llama3:8b", SizeVRAM: 4 * 1024 * 1024 * 1024}}
	nodes[0].VRAMSource = "nvidia"
	nodes[0].Unlock()

	return NewServer(r, nil, config.Config{})
}

// mockOllamaServer returns an httptest.Server that handles /api/tags.
func mockOllamaServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			// Two models: one small (fits), one large (won't fit).
			w.Write([]byte(`{"models":[
				{"name":"llama3:8b","size":4294967296},
				{"name":"llama3:70b","size":42949672960}
			]}`))
			return
		}
		http.NotFound(w, r)
	}))
}

func TestHandleModelFit_HappyPath(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/model-fit", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Nodes []struct {
			Name       string `json:"name"`
			VRAMSource string `json:"vram_source"`
			VRAMFree   int64  `json:"vram_free_bytes"`
			VRAMTotal  int64  `json:"vram_total_bytes"`
			GPUs       []struct {
				Name string `json:"name"`
			} `json:"gpus"`
			Models []struct {
				Name   string `json:"name"`
				Fit    string `json:"fit"`
				Loaded bool   `json:"loaded"`
			} `json:"models"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes count = %d, want 1", len(resp.Nodes))
	}
	node := resp.Nodes[0]
	if node.Name != "test-node" {
		t.Errorf("node.Name = %q, want test-node", node.Name)
	}
	if node.VRAMSource != "nvidia-smi" {
		t.Errorf("vram_source = %q, want nvidia-smi", node.VRAMSource)
	}
	if node.VRAMTotal != 8192*1024*1024 {
		t.Errorf("vram_total_bytes = %d, want %d", node.VRAMTotal, 8192*1024*1024)
	}
	if len(node.Models) != 2 {
		t.Fatalf("models count = %d, want 2", len(node.Models))
	}

	// llama3:8b is loaded and should fit (size * 1.15 < free * 0.85).
	m0 := node.Models[0]
	if m0.Name != "llama3:8b" {
		t.Errorf("models[0].name = %q, want llama3:8b", m0.Name)
	}
	if !m0.Loaded {
		t.Error("models[0].loaded = false, want true (it is in LoadedModels)")
	}
	// llama3:70b is 40 GB — won't fit in 8 GB node.
	m1 := node.Models[1]
	if m1.Name != "llama3:70b" {
		t.Errorf("models[1].name = %q, want llama3:70b", m1.Name)
	}
	if m1.Fit != "red" {
		t.Errorf("models[1].fit = %q, want red (70B won't fit in 8GB)", m1.Fit)
	}
}

func TestHandleModelFit_NodeUnreachable(t *testing.T) {
	// Point at a URL that will immediately refuse connections.
	s := newModelFitTestServer("http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/model-fit", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unreachable node returns empty models, not an error)", w.Code)
	}

	var resp struct {
		Nodes []struct {
			Models []interface{} `json:"models"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(resp.Nodes))
	}
	if len(resp.Nodes[0].Models) != 0 {
		t.Errorf("models = %d, want 0 (node unreachable)", len(resp.Nodes[0].Models))
	}
}

func TestHandleModelFit_V1Route(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/nodes/model-fit", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/admin/v1/ route: status = %d, want 200", w.Code)
	}
}

func TestHandleModelFit_Unauthorized(t *testing.T) {
	s := newModelFitTestServer("http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/model-fit", nil)
	// No auth header.
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
