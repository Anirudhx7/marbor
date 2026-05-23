package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newPullTestServer(t *testing.T, nodes []config.NodeConfig) *Server {
	t.Helper()
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: true,
			Keys: []config.KeyConfig{
				{Name: "test", Key: "test-token"},
			},
		},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, nodes, nil)
	return NewServer(r, nil, cfg)
}

func TestHandleNodePull_Success(t *testing.T) {
	var receivedBody []byte
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer mockOllama.Close()

	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: mockOllama.URL},
	})

	body := `{"model":"llama3:8b"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
	if resp["node"] != "gpu-0" {
		t.Errorf("expected node=gpu-0, got %v", resp["node"])
	}
	if resp["model"] != "llama3:8b" {
		t.Errorf("expected model=llama3:8b, got %v", resp["model"])
	}

	var forwarded map[string]interface{}
	if err := json.Unmarshal(receivedBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "llama3:8b" {
		t.Errorf("expected forwarded model=llama3:8b, got %v", forwarded["model"])
	}
	if forwarded["stream"] != false {
		t.Errorf("expected forwarded stream=false, got %v", forwarded["stream"])
	}
}

func TestHandleNodePull_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	body := `{"model":"llama3:8b"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/does-not-exist/pull", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "does-not-exist")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", res.StatusCode)
	}
}

func TestHandleNodePull_MissingModel(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/gpu-0/pull", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
}

func TestHandleNodePull_MethodNotAllowed(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/nodes/gpu-0/pull", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", res.StatusCode)
	}
}
