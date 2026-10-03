package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const threeNodeReplica = `{"replica_peers":{"members":["head","worker-a","worker-b"],"head":"head"}}`

// newRuntimeGuardServer builds a head + two workers + one standalone node,
// each with its own mock agent that counts calls, runtime control accepted
// and runtime.* capabilities present, backed by an in-memory store so audit
// rows can be read back.
func newRuntimeGuardServer(t *testing.T, declare bool) (*Server, *atomic.Int32, func()) {
	t.Helper()
	var hits atomic.Int32
	counting := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
	s, cleanup := newReplicaDeleteTestServer(t, []replicaDeleteTestNode{
		{name: "head", host: "host-head", handler: counting},
		{name: "worker-a", host: "host-a", handler: counting},
		{name: "worker-b", host: "host-b", handler: counting},
		{name: "solo", host: "host-solo", handler: counting},
	})
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.SetStore(st)
	for _, n := range s.router.Nodes() {
		n.Lock()
		n.AgentPresent = true
		n.AgentCapabilities = []string{"status", "runtime.start", "runtime.stop", "runtime.restart"}
		n.Unlock()
		s.router.SetNodeControl(n.Name, router.ControlConfig{Driver: "systemd", Identifier: "vllm.service", Configured: true})
	}
	if declare {
		for _, n := range []string{"head", "worker-a", "worker-b"} {
			patchReplicaPeers(t, s, n, threeNodeReplica)
		}
	}
	return s, &hits, cleanup
}

func runtimeReq(t *testing.T, s *Server, node, action, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRuntimeActionRequest(t, s, node, action)
	req.URL.RawQuery = query
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func drainReq(t *testing.T, s *Server, node string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/"+node+"/drain", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", node)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

func TestRuntimeAction_StandaloneUnchanged(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	for _, action := range []string{"stop", "restart", "start"} {
		w := runtimeReq(t, s, "solo", action, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", action, w.Code, w.Body.String())
		}
		if _, has := decodeBody(t, w)["replica"]; has {
			t.Errorf("%s on a standalone node must not carry a replica key: %s", action, w.Body.String())
		}
	}
	if hits.Load() != 3 {
		t.Errorf("agent hits = %d, want 3", hits.Load())
	}
}

func TestRuntimeAction_ReplicaMemberNeedsAcknowledge(t *testing.T) {
	cases := []struct {
		node, role, head string
	}{
		{"head", "head", "head"},
		{"worker-a", "worker", "head"},
	}
	for _, c := range cases {
		for _, action := range []string{"stop", "restart"} {
			t.Run(c.node+"/"+action, func(t *testing.T) {
				s, hits, cleanup := newRuntimeGuardServer(t, true)
				defer cleanup()

				w := runtimeReq(t, s, c.node, action, "")
				if w.Code != http.StatusConflict {
					t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
				}
				body := decodeBody(t, w)
				if body["code"] != "replica_member" {
					t.Errorf("code = %v, want replica_member", body["code"])
				}
				rep, _ := body["replica"].(map[string]any)
				if rep["role"] != c.role || rep["head"] != c.head {
					t.Errorf("replica = %v, want role %s head %s", rep, c.role, c.head)
				}
				if members, _ := rep["members"].([]any); len(members) != 3 {
					t.Errorf("members = %v, want 3", rep["members"])
				}
				if hits.Load() != 0 {
					t.Errorf("a rejected request must make zero agent calls, got %d", hits.Load())
				}

				w = runtimeReq(t, s, c.node, action, "acknowledge_replica=true")
				if w.Code != http.StatusOK {
					t.Fatalf("acknowledged status %d: %s", w.Code, w.Body.String())
				}
				rep, _ = decodeBody(t, w)["replica"].(map[string]any)
				if rep["role"] != c.role {
					t.Errorf("success replica = %v, want role %s", rep, c.role)
				}
				if hits.Load() != 1 {
					t.Errorf("agent hits = %d, want 1", hits.Load())
				}

				rows, err := s.st.QuerySystemAuditLog(10)
				if err != nil || len(rows) == 0 {
					t.Fatalf("audit rows: %v %v", rows, err)
				}
				detail := rows[0].Details
				if !strings.Contains(detail, "Replica role: "+c.role) || !strings.Contains(detail, "Replica head: head") {
					t.Errorf("audit detail %q should record role and head", detail)
				}
			})
		}
	}
}

func TestRuntimeAction_StartIsNotGuarded(t *testing.T) {
	s, _, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	w := runtimeReq(t, s, "head", "start", "")
	if w.Code != http.StatusOK {
		t.Fatalf("start on a head must not be guarded: %d %s", w.Code, w.Body.String())
	}
}

func TestRuntimeAction_UnreachableWorkerRejectedAsWorker(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	for _, n := range s.router.Nodes() {
		if n.Name == "worker-a" {
			n.Lock()
			n.AgentPresent = false
			n.AgentCapabilities = nil
			n.Unlock()
		}
	}
	w := runtimeReq(t, s, "worker-a", "stop", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (worker rejection before the agent check): %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Errorf("agent hits = %d, want 0", hits.Load())
	}
	// Acknowledged, the ordinary agent error applies.
	w = runtimeReq(t, s, "worker-a", "stop", "acknowledge_replica=true")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("acknowledged unreachable worker: status %d, want 503", w.Code)
	}
}

func TestRuntimeAction_UnresolvedMemberNeedsAcknowledge(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, false)
	defer cleanup()
	// "head" names "worker-a" as a member, but worker-a never declares back.
	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker-a"],"head":"head"}}`)

	w := runtimeReq(t, s, "worker-a", "restart", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	rep, _ := decodeBody(t, w)["replica"].(map[string]any)
	if rep["role"] != "unresolved" {
		t.Errorf("replica = %v, want role unresolved", rep)
	}
	if _, hasHead := rep["head"]; hasHead {
		t.Errorf("an unresolved member must not claim a head: %v", rep)
	}
	if hits.Load() != 0 {
		t.Errorf("agent hits = %d, want 0", hits.Load())
	}
	if w := runtimeReq(t, s, "worker-a", "restart", "acknowledge_replica=true"); w.Code != http.StatusOK {
		t.Errorf("acknowledged status %d: %s", w.Code, w.Body.String())
	}
}

func TestDrain_ReplicaWarningsAndStateUnchanged(t *testing.T) {
	cases := []struct {
		node, role, wantWarn string
	}{
		{"head", "head", "stops routing new requests to the whole replica"},
		{"worker-a", "worker", "has no routing effect"},
	}
	for _, c := range cases {
		t.Run(c.node, func(t *testing.T) {
			s, _, cleanup := newRuntimeGuardServer(t, true)
			defer cleanup()
			w := drainReq(t, s, c.node)
			if w.Code != http.StatusOK {
				t.Fatalf("drain is never blocked: status %d %s", w.Code, w.Body.String())
			}
			body := decodeBody(t, w)
			if body["draining"] != true || body["node"] != c.node {
				t.Errorf("drain response changed shape: %v", body)
			}
			rep, _ := body["replica"].(map[string]any)
			if rep["role"] != c.role {
				t.Errorf("replica = %v, want role %s", rep, c.role)
			}
			if warn, _ := rep["warning"].(string); !strings.Contains(warn, c.wantWarn) {
				t.Errorf("warning %q should contain %q", warn, c.wantWarn)
			}
			drained := false
			for _, n := range s.router.Nodes() {
				if n.Name == c.node {
					n.RLock()
					drained = n.Draining
					n.RUnlock()
				}
			}
			if !drained {
				t.Errorf("drain state must still be set on %s", c.node)
			}
		})
	}
}

func TestDrain_StandaloneUnchangedAndUnknown404(t *testing.T) {
	s, _, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	w := drainReq(t, s, "solo")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if _, has := decodeBody(t, w)["replica"]; has {
		t.Errorf("standalone drain must not carry a replica key: %s", w.Body.String())
	}
	if w := drainReq(t, s, "nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown node: status %d, want 404", w.Code)
	}
}

func TestDescribeReplicaMember_Table(t *testing.T) {
	mk := func(name string, members []string, head string) *router.NodeState {
		n := &router.NodeState{Name: name}
		if members != nil {
			n.ReplicaPeers = &store.ReplicaPeers{Members: members, Head: head}
		}
		return n
	}
	good := []string{"h", "w1", "w2"}
	nodes := []*router.NodeState{
		mk("h", good, "h"), mk("w1", good, "h"), mk("w2", good, "h"),
		mk("solo", nil, ""),
		mk("a", []string{"a", "b"}, "a"), mk("b", nil, ""), // b never declares back
	}
	r := router.New(config.RoutingConfig{}, nil, nil)
	roles, heads := r.SchedulingRolesWithHeads(nodes)

	cases := []struct {
		name, action string
		wantNil      bool
		role, head   string
		members      string
		warnContains string
	}{
		{name: "solo", action: "stop", wantNil: true},
		{name: "h", action: "stop", role: "head", head: "h", members: "h,w1,w2", warnContains: "takes the whole replica offline"},
		{name: "h", action: "restart", role: "head", head: "h", members: "h,w1,w2", warnContains: "restarting its runtime"},
		{name: "w1", action: "stop", role: "worker", head: "h", members: "h,w1,w2", warnContains: "headed by \"h\""},
		{name: "w1", action: "drain", role: "worker", head: "h", members: "h,w1,w2", warnContains: "no routing effect"},
		{name: "h", action: "drain", role: "head", head: "h", members: "h,w1,w2", warnContains: "whole replica"},
		{name: "b", action: "restart", role: "unresolved", members: "a,b", warnContains: "does not resolve"},
		{name: "b", action: "drain", role: "unresolved", members: "a,b", warnContains: "cannot be determined"},
	}
	for _, c := range cases {
		got := describeReplicaMember(c.name, c.action, nodes, roles, heads)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s/%s: want nil, got %+v", c.name, c.action, got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("%s/%s: nil", c.name, c.action)
		}
		if got.Role != c.role || got.Head != c.head || strings.Join(got.Members, ",") != c.members {
			t.Errorf("%s/%s: got %+v", c.name, c.action, got)
		}
		if !strings.Contains(got.Warning, c.warnContains) {
			t.Errorf("%s/%s: warning %q lacks %q", c.name, c.action, got.Warning, c.warnContains)
		}
		if strings.Contains(got.Warning, "rejoin") {
			t.Errorf("%s/%s: warning must not claim members rejoin: %q", c.name, c.action, got.Warning)
		}
	}
}
