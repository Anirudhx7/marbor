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

// TestHandleUnloadModel_PinnedRejected verifies the operator-facing
// POST /admin/nodes/{name}/unload endpoint rejects a pinned model with 409
// Conflict instead of forwarding keep_alive:0 to the node. Pinning is
// supposed to mean "never evict/unload without an explicit unpin first", and
// that must hold on this manual path exactly like it does on auto-eviction.
func TestHandleUnloadModel_PinnedRejected(t *testing.T) {
	hit := false
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"done":true}`))
	}))
	defer mockOllama.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: mockOllama.URL}}, nil)
	r.SetPinnedModels("gpu-0", []string{"guarded-model"})
	s := NewServer(r, nil, config.Config{})

	body := `{"model":"guarded-model"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/unload", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("expected 409, got %d (body: %s)", res.StatusCode, b)
	}
	var resp map[string]string
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "pinned") {
		t.Errorf("expected error message to mention pinning, got %q", resp["error"])
	}
	if hit {
		t.Error("pinned model unload must not reach the node")
	}
}

// TestHandleUnloadModel_NonPinnedStillWorks verifies the fix doesn't regress
// ordinary unloads: a non-pinned model still returns 200 and forwards
// keep_alive:0 to the node exactly as before.
func TestHandleUnloadModel_NonPinnedStillWorks(t *testing.T) {
	var receivedBody []byte
	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = b
		w.Write([]byte(`{"done":true}`))
	}))
	defer mockOllama.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: mockOllama.URL}}, nil)
	r.SetPinnedModels("gpu-0", []string{"some-other-model"})
	s := NewServer(r, nil, config.Config{})

	body := `{"model":"llama3:8b"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-0/unload", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "gpu-0")

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("expected 200, got %d (body: %s)", res.StatusCode, b)
	}
	if !strings.Contains(string(receivedBody), `"keep_alive":0`) {
		t.Errorf("expected forwarded keep_alive:0, got %s", receivedBody)
	}
}
