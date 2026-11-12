package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/Anirudhx7/marbor/internal/router"
)

func setBenchNodeRuntime(s *Server, runtime string) {
	for _, n := range s.router.Nodes() {
		n.Lock()
		n.Runtime = runtime
		n.Unlock()
	}
}

// newBenchAgentServer returns a server whose only node has no loaded models but
// an enabled agent advertising models.list, backed by a mock listing "llama3:latest".
func newBenchAgentServer(t *testing.T, runtime string) *Server {
	t.Helper()
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3:latest","size_bytes":111,"source":"ollama-tags"}]}`))
	}))
	t.Cleanup(mockAgent.Close)
	u, err := url.Parse(mockAgent.URL)
	if err != nil {
		t.Fatal(err)
	}
	agentPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	s := newBenchTestServer(t, "http://127.0.0.1:1", "", "")
	agentHost, ok := s.router.NodeHost("gpu-0")
	if !ok {
		t.Fatal("node gpu-0 has no host")
	}
	s.router.SetMarborAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range s.router.Nodes() {
		n.Lock()
		n.Runtime = runtime
		n.AgentCapabilities = []string{"status", "models.pull", "models.list"}
		n.Unlock()
	}
	return s
}

func TestNodeHasModel_BareNameMatchesLatestOnOllama(t *testing.T) {
	for _, runtime := range []string{"ollama", ""} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			s := newBenchTestServer(t, "http://127.0.0.1:1", "", "llama3:latest")
			setBenchNodeRuntime(s, runtime)
			if !s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
				t.Error("bare name should match the resident :latest copy")
			}
			if !s.nodeHasModel(context.Background(), "gpu-0", "llama3:latest") {
				t.Error("exact name should match")
			}
			if s.nodeHasModel(context.Background(), "gpu-0", "llama3:8b") {
				t.Error("a different tag must not match")
			}
		})
	}
}

func TestNodeHasModel_NonOllamaStaysExact(t *testing.T) {
	for _, runtime := range []string{"vllm", "llama.cpp", "mlx"} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			s := newBenchTestServer(t, "http://127.0.0.1:1", "", "llama3:latest")
			setBenchNodeRuntime(s, runtime)
			if s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
				t.Error("bare name must not match a :latest id")
			}
			if !s.nodeHasModel(context.Background(), "gpu-0", "llama3:latest") {
				t.Error("exact id should match")
			}
		})
	}
}

// The agent models.list branch (nothing loaded, agent lists the model) applies
// the same runtime-gated matching.
func TestNodeHasModel_AgentListBareNameMatchesLatestOnOllama(t *testing.T) {
	for _, runtime := range []string{"ollama", ""} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			s := newBenchAgentServer(t, runtime)
			if !s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
				t.Error("bare name should match the agent-listed :latest copy")
			}
			if s.nodeHasModel(context.Background(), "gpu-0", "llama3:8b") {
				t.Error("a different tag must not match")
			}
		})
	}
}

func TestNodeHasModel_AgentListNonOllamaStaysExact(t *testing.T) {
	for _, runtime := range []string{"vllm", "llama.cpp", "mlx"} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			s := newBenchAgentServer(t, runtime)
			if s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
				t.Error("bare name must not match an agent-listed :latest id")
			}
			if !s.nodeHasModel(context.Background(), "gpu-0", "llama3:latest") {
				t.Error("exact id should match")
			}
		})
	}
}

func TestModelNamesMatchOnRuntime_Exported(t *testing.T) {
	if !router.ModelNamesMatchOnRuntime("ollama", "a", "a:latest") {
		t.Error("ollama should treat bare and :latest as one model")
	}
	if router.ModelNamesMatchOnRuntime("llama.cpp", "a", "a:latest") {
		t.Error("llama.cpp should be exact")
	}
}

// Registry host:port is not a tag, a different tag never matches, and a
// digest-pinned name matches only itself.
func TestNodeHasModel_RegistryAndDigestNames(t *testing.T) {
	for _, tc := range []struct {
		resident, ask string
		want          bool
	}{
		{"host:5000/m:latest", "host:5000/m", true},
		{"host:5000/m", "host:5000/m:latest", true},
		{"host:5000/m:latest", "host:5000/m:v1", false},
		{"m@sha256:abc", "m", false},
		{"m@sha256:abc", "m:latest", false},
		{"m@sha256:abc", "m@sha256:abc", true},
	} {
		s := newBenchTestServer(t, "http://127.0.0.1:1", "", tc.resident)
		setBenchNodeRuntime(s, "ollama")
		if got := s.nodeHasModel(context.Background(), "gpu-0", tc.ask); got != tc.want {
			t.Errorf("resident %q, ask %q: got %t, want %t", tc.resident, tc.ask, got, tc.want)
		}
	}
}

func TestNodeHasModel_UnknownNodeHasNothing(t *testing.T) {
	s := newBenchTestServer(t, "http://127.0.0.1:1", "", "llama3:latest")
	if s.nodeHasModel(context.Background(), "ghost", "llama3") {
		t.Error("an unknown node must report no model")
	}
}
