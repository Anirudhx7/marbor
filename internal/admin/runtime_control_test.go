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

// TestNodeAgentInstallCommandUsesNewContract verifies the generated Node Agent
// install commands use the renamed Marbor Agent environment-variable contract
// (MARBOR_SERVER / MARBOR_ENROLL) and never the legacy TOKEN / MESH / ENROLL
// names. A regression here silently hands operators an install command that the
// agent binary (which only reads MARBOR_*) cannot consume. ROLE and PORT are
// installer-specific arguments and are intentionally still present.
func TestNodeAgentInstallCommandUsesNewContract(t *testing.T) {
	unix, windows := nodeAgentInstallCommand("https://marbor.example.com", 11434, "abc123")

	for name, cmd := range map[string]string{"unix": unix, "windows": windows} {
		if !strings.Contains(cmd, "MARBOR_SERVER=") {
			t.Errorf("%s install command missing MARBOR_SERVER: %q", name, cmd)
		}
		if !strings.Contains(cmd, "MARBOR_ENROLL=") {
			t.Errorf("%s install command missing MARBOR_ENROLL: %q", name, cmd)
		}
		for _, legacy := range []string{"TOKEN=", "MESH=", " ENROLL="} {
			if strings.Contains(cmd, legacy) {
				t.Errorf("%s install command still contains legacy %q: %q", name, legacy, cmd)
			}
		}
	}
}

func newRuntimeActionRequest(t *testing.T, s *Server, node, action string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/"+node+"/runtime/"+action, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	return req
}

// TestHandleNodeRuntimeAction_DispatchesToAgentWhenConfigured verifies the
// success path: capability present, control driver accepted, mesh dispatches
// POST /v1/runtime/{action} to the agent with {driver, identifier}.
func TestHandleNodeRuntimeAction_DispatchesToAgentWhenConfigured(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]interface{}
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
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
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.start", "runtime.stop", "runtime.restart"}
			n.Unlock()
		}
	}
	r.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "restart"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected agent request method POST, got %q", gotMethod)
	}
	if gotPath != "/v1/runtime/restart" {
		t.Errorf("expected agent request path /v1/runtime/restart, got %q", gotPath)
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}
	if gotBody["driver"] != "systemd" || gotBody["identifier"] != "ollama.service" {
		t.Errorf("agent request body = %+v, want driver=systemd identifier=ollama.service", gotBody)
	}
}

// TestHandleNodeRuntimeAction_WorksWhileRuntimeUnhealthy is the exact
// scenario Runtime Control exists for: an operator stops the runtime, the
// router's next poll of the (now intentionally down) runtime URL marks
// n.Healthy false, and the operator then clicks Start. This must still
// dispatch to the agent - gating on runtime health here would make "start"
// permanently unreachable the moment "stop" ever succeeded once.
func TestHandleNodeRuntimeAction_WorksWhileRuntimeUnhealthy(t *testing.T) {
	var gotPath string
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockAgent.Close()
	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.Healthy = false // the runtime is intentionally stopped
			n.AgentCapabilities = []string{"status", "runtime.start"}
			n.Unlock()
		}
	}
	r.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})
	s := NewServer(r, nil, config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{{Name: "t", Key: "test-token"}}},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "start"))

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (start must work on an unhealthy/stopped runtime), got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if gotPath != "/v1/runtime/start" {
		t.Errorf("expected agent request path /v1/runtime/start, got %q", gotPath)
	}
}

// TestHandleNodeRuntimeAction_NoAgentCapabilityReturns501 verifies a node
// without the runtime.{action} capability gets a clear, honest error - never
// a fabricated success for an action that never ran (R1).
func TestHandleNodeRuntimeAction_NoAgentCapabilityReturns501(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})
	for _, n := range s.router.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.Unlock()
		}
	}
	s.router.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "stop"))

	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

// TestHandleNodeRuntimeAction_UnconfiguredNodeReturns422 is the safety-
// critical branch this design exists to protect: a node with a real agent
// capability but no operator-accepted control driver must return the exact
// error node-agent-capabilities.md section 5.6 mandates, never guess one.
func TestHandleNodeRuntimeAction_UnconfiguredNodeReturns422(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("agent should never be dispatched to for an unconfigured node")
	}))
	defer mockAgent.Close()
	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.start", "runtime.stop", "runtime.restart"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{{Name: "t", Key: "test-token"}}},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "start"))

	if w.Result().StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "Runtime control unavailable: no control driver configured" {
		t.Errorf("error = %v, want the exact mandated message", body["error"])
	}
}

// TestHandleNodeRuntimeLogs_DispatchesToAgentWhenConfigured verifies the
// success path: capability present, control driver accepted, mesh dispatches
// POST /v1/runtime/logs to the agent with {driver, identifier, lines} and
// relays the returned lines, never audit-logging a pure read.
func TestHandleNodeRuntimeLogs_DispatchesToAgentWhenConfigured(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]interface{}
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"lines":["line one","line two"]}`))
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
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.logs"}
			n.Unlock()
		}
	}
	r.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})
	s := NewServer(r, nil, cfg)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "logs"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected agent request method POST, got %q", gotMethod)
	}
	if gotPath != "/v1/runtime/logs" {
		t.Errorf("expected agent request path /v1/runtime/logs, got %q", gotPath)
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}
	if gotBody["driver"] != "systemd" || gotBody["identifier"] != "ollama.service" {
		t.Errorf("agent request body = %+v, want driver=systemd identifier=ollama.service", gotBody)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lines, ok := body["lines"].([]interface{})
	if !ok || len(lines) != 2 {
		t.Fatalf("lines = %v, want 2 lines", body["lines"])
	}
}

// TestHandleNodeRuntimeLogs_NoAgentCapabilityReturns501 mirrors
// TestHandleNodeRuntimeAction_NoAgentCapabilityReturns501 for the logs
// capability - a node without runtime.logs gets a clear, honest error, never
// a fabricated empty log list (R1).
func TestHandleNodeRuntimeLogs_NoAgentCapabilityReturns501(t *testing.T) {
	s := newPullTestServer(t, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	})
	for _, n := range s.router.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.Unlock()
		}
	}
	s.router.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "logs"))

	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

// TestHandleNodeRuntimeLogs_UnconfiguredNodeReturns422 mirrors the
// start/stop/restart safety-critical branch for logs.
func TestHandleNodeRuntimeLogs_UnconfiguredNodeReturns422(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("agent should never be dispatched to for an unconfigured node")
	}))
	defer mockAgent.Close()
	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.logs"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{{Name: "t", Key: "test-token"}}},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "logs"))

	if w.Result().StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "Runtime control unavailable: no control driver configured" {
		t.Errorf("error = %v, want the exact mandated message", body["error"])
	}
}

// TestHandleNodeRuntimeLogs_AgentErrorPassthrough mirrors the
// start/stop/restart passthrough test for logs.
func TestHandleNodeRuntimeLogs_AgentErrorPassthrough(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"process: log retrieval not supported without a supervisor"}`))
	}))
	defer mockAgent.Close()
	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.logs"}
			n.Unlock()
		}
	}
	r.SetNodeControl("gpu-0", router.ControlConfig{Driver: "process", Identifier: "/var/run/ollama.pid", Configured: true})
	s := NewServer(r, nil, config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{{Name: "t", Key: "test-token"}}},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "logs"))

	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "not supported without a supervisor") {
		t.Errorf("error = %v, want it to contain the agent's real error text", body["error"])
	}
}

// TestHandleNodeRuntimeAction_AgentErrorPassthrough verifies a real driver
// execution failure on the agent side is relayed as a 502 with the agent's
// own error text, not swallowed or replaced with a generic message.
func TestHandleNodeRuntimeAction_AgentErrorPassthrough(t *testing.T) {
	mockAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"ok":false,"error":"systemd: restart ollama.service: Unit not found"}`))
	}))
	defer mockAgent.Close()
	agentPort := 0
	fmt.Sscanf(strings.TrimPrefix(mockAgent.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:11434"},
	}, nil)
	// nodeAgents is keyed by the node's shared Host now, not its Name - see
	// router.SetNodeAgent's doc comment.
	agentHost, _ := r.NodeHost("gpu-0")
	r.SetNodeAgent(agentHost, true, agentPort, "agent-secret-token", "http")
	for _, n := range r.Nodes() {
		if n.Name == "gpu-0" {
			n.Lock()
			n.AgentPresent = true
			n.AgentCapabilities = []string{"status", "runtime.restart"}
			n.Unlock()
		}
	}
	r.SetNodeControl("gpu-0", router.ControlConfig{Driver: "systemd", Identifier: "ollama.service", Configured: true})
	s := NewServer(r, nil, config.Config{
		Auth: config.AuthConfig{Enabled: config.BoolPtr(true), Keys: []config.KeyConfig{{Name: "t", Key: "test-token"}}},
	})

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newRuntimeActionRequest(t, s, "gpu-0", "restart"))

	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "Unit not found") {
		t.Errorf("error = %v, want it to contain the agent's real error text", body["error"])
	}
}
