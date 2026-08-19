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

func newDeleteModelRequest(t *testing.T, s *Server, node, model string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/nodes/"+node+"/models/"+model, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	req.SetPathValue("model", model)
	return req
}

// TestHandleNodeDeleteModel_DispatchesToAgentWhenCapable verifies the mesh
// forwards to the node's Node Agent (DELETE /v1/models/{name}, capability
// "models.delete") - mirroring TestHandleNodeModels_DispatchesToAgentWhenCapable
// for the list capability.
func TestHandleNodeDeleteModel_DispatchesToAgentWhenCapable(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
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
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentCapabilities = []string{"status", "models.pull", "models.list", "models.delete"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "gpu-0", "org/repo"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		body, _ := json.Marshal(w.Body.String())
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, body)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected agent request method DELETE, got %q", gotMethod)
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
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
}

// TestHandleNodeDeleteModel_NoAgentCapabilityReturns501 verifies a node
// without the agent capability gets a clear, honest error - never a
// fabricated success for a delete that never happened (R1).
func TestHandleNodeDeleteModel_NoAgentCapabilityReturns501(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "gpu-0", "org/repo"))

	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Result().StatusCode)
	}
}

func TestHandleNodeDeleteModel_NodeNotFound(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "does-not-exist", "org/repo"))

	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Result().StatusCode)
	}
}

// TestHandleNodeDeleteModel_DownNodeFailsFast mirrors
// TestHandleNodePull_DownNodeFailsFast: a down node must be rejected with a
// clear reason before ever attempting the agent dispatch.
func TestHandleNodeDeleteModel_DownNodeFailsFast(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})

	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = false
	nodes[0].Unlock()

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "gpu-0", "org/repo"))

	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Result().StatusCode)
	}
}

// TestBuildAgentDeleteURL_EscapesReservedCharacters verifies a model name
// containing a URL-reserved character ('#', '?', a space) is percent-escaped
// rather than passed through verbatim - unescaped, those characters get
// reinterpreted as a fragment/query boundary by url.Parse, truncating the
// request to a different (shorter) model name than the caller intended.
// "/" must still pass through as a real path separator (unescaped),
// matching the agent's own "{name...}" wildcard route.
func TestBuildAgentURL_EscapesReservedModelPathCharacters(t *testing.T) {
	cases := map[string]string{
		"org/repo":     "http://localhost:9911/v1/models/org/repo",
		"org/repo#tag": "http://localhost:9911/v1/models/org/repo%23tag",
		"org/repo?x=1": "http://localhost:9911/v1/models/org/repo%3Fx=1",
		"org/my repo":  "http://localhost:9911/v1/models/org/my%20repo",
	}
	for model, want := range cases {
		got, err := buildAgentURL("http://localhost:11434", 9911, "http", "/v1/models/"+escapeModelPathSegments(model))
		if err != nil {
			t.Fatalf("buildAgentURL(%q): %v", model, err)
		}
		if got != want {
			t.Errorf("buildAgentURL(%q) = %q, want %q", model, got, want)
		}
	}
}
