package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func newDeleteModelRequest(t *testing.T, s *Server, node, model string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/admin/v1/nodes/"+node+"/models/"+model, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	req.SetPathValue("model", model)
	return req
}

// TestHandleNodeDeleteModel_DispatchesToAgentWhenCapable verifies the marbor
// forwards to the node's Marbor Agent (DELETE /v1/models/{name}, capability
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
	r.SetMarborAgent(agentHost, true, agentPort, "agent-secret-token", "http")
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
	if _, hasReplica := resp["replica"]; hasReplica {
		t.Errorf("standalone delete response must not carry a %q field, got %v", "replica", resp["replica"])
	}
}

// TestHandleNodeDeleteModel_NoAgentCapabilityReturns501 verifies a node
// without the agent capability gets a clear, honest error - never a
// fabricated success for a delete that never happened.
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
// replicaDeleteTestNode describes one node in a replica-delete test fixture:
// its own name, the host key used for its (independent) Marbor Agent
// config, and the handler its mock agent responds with.
type replicaDeleteTestNode struct {
	name    string
	host    string
	handler http.HandlerFunc
}

// newReplicaDeleteTestServer builds a router with one node per entry in
// nodes, each wired to its own independent mock agent (distinct Host key,
// see config.NodeConfig.Host's "groups this node with any other node on the
// same host" doc comment - required so three separate nodes sharing the
// "localhost" hostname their mock agents actually listen on don't collide
// on a single shared marbor-agent config). Returns the server and a cleanup
// func that closes every mock agent server.
func newReplicaDeleteTestServer(t *testing.T, nodes []replicaDeleteTestNode) (*Server, func()) {
	t.Helper()
	var nodeConfigs []config.NodeConfig
	var mockServers []*httptest.Server
	agentPorts := make(map[string]int, len(nodes))
	for _, n := range nodes {
		mock := httptest.NewServer(n.handler)
		mockServers = append(mockServers, mock)
		var port int
		fmt.Sscanf(strings.TrimPrefix(mock.URL, "http://127.0.0.1:"), "%d", &port)
		agentPorts[n.name] = port
		nodeConfigs = append(nodeConfigs, config.NodeConfig{Name: n.name, URL: "http://localhost:11434", Host: n.host, Runtime: "vllm"})
	}

	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, nodeConfigs, nil)
	for _, n := range nodes {
		r.SetMarborAgent(n.host, true, agentPorts[n.name], "agent-secret-token-"+n.name, "http")
	}
	for _, rn := range r.Nodes() {
		rn.Lock()
		rn.AgentCapabilities = []string{"status", "models.pull", "models.list", "models.delete"}
		rn.Unlock()
	}
	s := NewServer(r, nil, cfg)

	cleanup := func() {
		for _, mock := range mockServers {
			mock.Close()
		}
	}
	return s, cleanup
}

func patchReplicaPeers(t *testing.T, s *Server, name, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name, bytes.NewReader([]byte(body)))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
	}
}

func okAgentHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

// TestHandleNodeDeleteModel_HeadTriggersReplicaWideDelete verifies a delete
// issued against a resolved replica HEAD expands into one delete per
// member (head included), every member is dispatched exactly once
// (concurrently - wire-arrival order is not asserted, only that each
// member is hit), and the response carries the aggregated member/results
// list in deterministic (sorted, not dispatch-order-dependent) order.
func TestHandleNodeDeleteModel_HeadTriggersReplicaWideDelete(t *testing.T) {
	var mu sync.Mutex
	hitCount := map[string]int{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hitCount[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}
	}
	s, cleanup := newReplicaDeleteTestServer(t, []replicaDeleteTestNode{
		{name: "head", host: "host-head", handler: handler("head")},
		{name: "worker-b", host: "host-worker-b", handler: handler("worker-b")},
		{name: "worker-a", host: "host-worker-a", handler: handler("worker-a")},
	})
	defer cleanup()

	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)
	patchReplicaPeers(t, s, "worker-a", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)
	patchReplicaPeers(t, s, "worker-b", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "head", "org/repo"))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, w.Body.String())
	}

	var resp struct {
		OK      bool                     `json:"ok"`
		Replica bool                     `json:"replica"`
		Head    string                   `json:"head"`
		Members []string                 `json:"members"`
		Results []nodeDeleteMemberResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.Replica || resp.Head != "head" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	wantMembers := []string{"head", "worker-a", "worker-b"}
	if !reflect.DeepEqual(resp.Members, wantMembers) {
		t.Errorf("members = %v, want sorted %v", resp.Members, wantMembers)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}
	for i, res := range resp.Results {
		if !res.OK || res.Node != wantMembers[i] {
			t.Errorf("results[%d] = %+v, want ok=true node=%q", i, res, wantMembers[i])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range wantMembers {
		if hitCount[name] != 1 {
			t.Errorf("member %q hit %d times, want exactly 1", name, hitCount[name])
		}
	}
}

// TestHandleNodeDeleteModel_HeadReplicaWide_PartialFailure verifies a
// failure on one member does not stop the others from being attempted, the
// top-level response is 502 (never a silently-successful 200), and the
// aggregate message states how many members succeeded.
func TestHandleNodeDeleteModel_HeadReplicaWide_PartialFailure(t *testing.T) {
	s, cleanup := newReplicaDeleteTestServer(t, []replicaDeleteTestNode{
		{name: "head", host: "host-head", handler: okAgentHandler(t)},
		{name: "worker-a", host: "host-worker-a", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":false,"error":"disk busy"}`))
		}},
		{name: "worker-b", host: "host-worker-b", handler: okAgentHandler(t)},
	})
	defer cleanup()

	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)
	patchReplicaPeers(t, s, "worker-a", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)
	patchReplicaPeers(t, s, "worker-b", `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "head", "org/repo"))

	res := w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", res.StatusCode, w.Body.String())
	}

	var resp struct {
		OK      bool                     `json:"ok"`
		Error   string                   `json:"error"`
		Results []nodeDeleteMemberResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Errorf("expected ok=false on partial failure, got true")
	}
	if !strings.Contains(resp.Error, "2 of 3 members succeeded") {
		t.Errorf("error = %q, want aggregate stating 2 of 3 members succeeded", resp.Error)
	}
	byNode := make(map[string]nodeDeleteMemberResult, len(resp.Results))
	for _, r := range resp.Results {
		byNode[r.Node] = r
	}
	if len(byNode) != 3 {
		t.Fatalf("expected all 3 members in results (later member must still be attempted after an earlier failure), got %d: %+v", len(byNode), resp.Results)
	}
	if byNode["head"].OK != true || byNode["worker-b"].OK != true {
		t.Errorf("head and worker-b should have succeeded: %+v", resp.Results)
	}
	if byNode["worker-a"].OK != false || !strings.Contains(byNode["worker-a"].Error, "disk busy") {
		t.Errorf("worker-a should have failed with the agent's own error text: %+v", byNode["worker-a"])
	}
}

// TestHandleNodeDeleteModel_WorkerRejected409 verifies a worker node is
// rejected before any health/capability check or agent call, naming both
// its own role and the resolved head in the error.
func TestHandleNodeDeleteModel_WorkerRejected409(t *testing.T) {
	agentCalled := false
	s, cleanup := newReplicaDeleteTestServer(t, []replicaDeleteTestNode{
		{name: "head", host: "host-head", handler: okAgentHandler(t)},
		{name: "worker", host: "host-worker", handler: func(w http.ResponseWriter, r *http.Request) {
			agentCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}},
	})
	defer cleanup()

	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker"],"head":"head"}}`)
	patchReplicaPeers(t, s, "worker", `{"replica_peers":{"members":["head","worker"],"head":"head"}}`)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "worker", "org/repo"))

	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "worker") || !strings.Contains(body, "head") {
		t.Errorf("error body should name the node's role and resolved head, got %s", body)
	}
	if agentCalled {
		t.Errorf("worker rejection must make zero agent calls")
	}
}

// TestHandleNodeDeleteModel_UnresolvedRejected409 mirrors
// admin_replica_test.go's one-sided-declaration pattern: a node pulled into
// an unresolved component by another node's declaration it never
// reciprocated must be rejected too, with no head name claimed (none is
// resolvable) and zero agent calls.
func TestHandleNodeDeleteModel_UnresolvedRejected409(t *testing.T) {
	agentCalled := false
	s, cleanup := newReplicaDeleteTestServer(t, []replicaDeleteTestNode{
		{name: "standalone", host: "host-standalone", handler: okAgentHandler(t)},
		{name: "one-sided", host: "host-one-sided", handler: func(w http.ResponseWriter, r *http.Request) {
			agentCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}},
	})
	defer cleanup()

	// "standalone" names "one-sided" as a replica member, but "one-sided"
	// never declares anything back - the whole component is unresolved.
	patchReplicaPeers(t, s, "standalone", `{"replica_peers":{"members":["standalone","one-sided"],"head":"standalone"}}`)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newDeleteModelRequest(t, s, "one-sided", "org/repo"))

	if w.Result().StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if agentCalled {
		t.Errorf("unresolved rejection must make zero agent calls")
	}
}

// TestValidateReplicaDeleteComponent_FailsClosedOnInconsistency unit-tests
// the defensive component-validation guard directly with hand-built,
// deliberately inconsistent inputs. A real resolveSchedulingRolesAndHeads
// run should never produce an inconsistent component for a node already
// resolved to RoleHead - this proves the guard itself rejects such a state
// (rather than trusting that invariant blindly) and makes zero agent calls
// possible from it, independent of whether that state is reachable through
// normal PATCH operations today.
func TestValidateReplicaDeleteComponent_FailsClosedOnInconsistency(t *testing.T) {
	mkNode := func(name string) *router.NodeState {
		return &router.NodeState{Name: name, ReplicaPeers: &store.ReplicaPeers{
			Members: []string{"head", "worker"}, Head: "head",
		}}
	}
	head := mkNode("head")
	worker := mkNode("worker")
	allNodes := []*router.NodeState{head, worker}

	tests := []struct {
		name  string
		roles map[string]router.SchedulingRole
		heads map[string]string
	}{
		{
			name:  "member role unresolved",
			roles: map[string]router.SchedulingRole{"head": router.RoleHead, "worker": router.RoleUnresolved},
			heads: map[string]string{"head": "head", "worker": "head"},
		},
		{
			name:  "member resolves to a different head",
			roles: map[string]router.SchedulingRole{"head": router.RoleHead, "worker": router.RoleWorker},
			heads: map[string]string{"head": "head", "worker": "some-other-head"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members, reason := validateReplicaDeleteComponent("head", allNodes, tt.roles, tt.heads)
			if reason == "" {
				t.Fatalf("expected a non-empty rejection reason, got members=%v reason=%q", members, reason)
			}
			if members != nil {
				t.Errorf("expected no members on a failed validation, got %v", members)
			}
		})
	}
}

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
