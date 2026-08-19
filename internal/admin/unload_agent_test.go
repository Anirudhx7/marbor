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

func newUnloadRequest(t *testing.T, s *Server, node, model string) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q}`, model)
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/"+node+"/unload", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", node)
	return req
}

// newAgentUnloadTestServer builds a router with one node whose Node Agent
// reports "models.unload", pointed at mockAgent, mirroring
// TestHandleNodeDeleteModel_DispatchesToAgentWhenCapable's fixture.
func newAgentUnloadTestServer(t *testing.T, mockAgent *httptest.Server) *Server {
	t.Helper()
	var agentPort int
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
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentCapabilities = []string{"status", "models.pull", "models.list", "models.delete", "models.unload"}
			n.Unlock()
		}
	}
	return NewServer(r, nil, cfg)
}

// TestHandleUnloadModel_DispatchesToAgentWhenCapable verifies the mesh
// forwards to the node's Node Agent (POST /v1/models/{name}, capability
// "models.unload") instead of Ollama's own keep_alive:0 HTTP trick, when the
// node reports the capability - mirroring
// TestHandleNodeDeleteModel_DispatchesToAgentWhenCapable for delete.
func TestHandleUnloadModel_DispatchesToAgentWhenCapable(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockAgent.Close()

	s := newAgentUnloadTestServer(t, mockAgent)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newUnloadRequest(t, s, "gpu-0", "org/repo"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected agent request method POST, got %q", gotMethod)
	}
	if gotPath != "/v1/models/org/repo" {
		t.Errorf("expected agent request path /v1/models/org/repo, got %q", gotPath)
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["unloaded"] != true {
		t.Errorf("expected unloaded=true, got %v", resp["unloaded"])
	}
}

// TestHandleUnloadModel_AgentPinnedRejected verifies a pinned model is
// rejected with 409 before the agent is ever contacted, even when the node
// has the "models.unload" capability - pinning must be honored identically
// on the agent path, not just the pre-existing direct path.
func TestHandleUnloadModel_AgentPinnedRejected(t *testing.T) {
	hit := false
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockAgent.Close()

	s := newAgentUnloadTestServer(t, mockAgent)
	s.router.SetPinnedModels("gpu-0", []string{"guarded-model"})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newUnloadRequest(t, s, "gpu-0", "guarded-model"))

	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Result().StatusCode)
	}
	if hit {
		t.Error("pinned model unload must not reach the agent")
	}
}

// TestHandleUnloadModel_AgentUnsupportedRuntimeReturnsError verifies a real
// error from the agent (e.g. a non-Ollama runtime with no unload primitive)
// surfaces as a bad-gateway error, never a fabricated success (R1) - this is
// what makes "not supported for this runtime" visible in the UI instead of a
// silent no-op.
func TestHandleUnloadModel_AgentUnsupportedRuntimeReturnsError(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"ok":false,"error":"unsupported: no unload primitive for runtime \"vllm\""}`))
	}))
	defer mockAgent.Close()

	s := newAgentUnloadTestServer(t, mockAgent)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newUnloadRequest(t, s, "gpu-0", "some-model"))

	res := w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", res.StatusCode)
	}
	var resp map[string]string
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "unsupported") {
		t.Errorf("expected error to mention 'unsupported', got %q", resp["error"])
	}
}

// TestHandleUnloadModel_AgentDownNodeFailsFast mirrors
// TestHandleNodeDeleteModel_DownNodeFailsFast: a down node must be rejected
// with a clear reason before ever attempting the agent dispatch.
func TestHandleUnloadModel_AgentDownNodeFailsFast(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent must not be contacted for a down node")
	}))
	defer mockAgent.Close()

	s := newAgentUnloadTestServer(t, mockAgent)
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = false
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newUnloadRequest(t, s, "gpu-0", "some-model"))

	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Result().StatusCode)
	}
}

// TestHandleUnloadModel_AgentCapableNodeNotFound verifies an unknown node
// name still 404s cleanly (checked ahead of both the pin check and the
// agent-capability branch).
func TestHandleUnloadModel_AgentCapableNodeNotFound(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent must not be contacted for an unknown node")
	}))
	defer mockAgent.Close()

	s := newAgentUnloadTestServer(t, mockAgent)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newUnloadRequest(t, s, "does-not-exist", "some-model"))

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}

// TestBuildAgentUnloadURL_EscapesReservedCharacters mirrors
// TestBuildAgentDeleteURL_EscapesReservedCharacters: a model name containing
// a URL-reserved character must be percent-escaped per path segment, not
// passed through verbatim, while "/" still passes through unescaped to
// match the agent's "{name...}" wildcard route.
func TestBuildAgentUnloadURL_EscapesReservedCharacters(t *testing.T) {
	cases := map[string]string{
		"org/repo":     "http://localhost:9911/v1/models/org/repo",
		"org/repo#tag": "http://localhost:9911/v1/models/org/repo%23tag",
		"org/repo?x=1": "http://localhost:9911/v1/models/org/repo%3Fx=1",
		"org/my repo":  "http://localhost:9911/v1/models/org/my%20repo",
	}
	for model, want := range cases {
		got, err := buildAgentUnloadURL("http://localhost:11434", 9911, "http", model)
		if err != nil {
			t.Fatalf("buildAgentUnloadURL(%q): %v", model, err)
		}
		if got != want {
			t.Errorf("buildAgentUnloadURL(%q) = %q, want %q", model, got, want)
		}
	}
}
