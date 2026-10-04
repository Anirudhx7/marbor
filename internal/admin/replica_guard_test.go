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
	return newRuntimeGuardServerFailing(t, declare, nil)
}

// newRuntimeGuardServerFailing is newRuntimeGuardServer with every agent
// answering 500 while fail is true (a nil fail never fails).
func newRuntimeGuardServerFailing(t *testing.T, declare bool, fail *atomic.Bool) (*Server, *atomic.Int32, func()) {
	t.Helper()
	var hits atomic.Int32
	counting := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail != nil && fail.Load() {
			http.Error(w, `{"error":"agent exploded"}`, http.StatusInternalServerError)
			return
		}
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

// runtimeAuditCount counts runtime_* and drain_node audit rows. The in-memory
// store is shared across servers in this package, so tests compare against a
// baseline rather than expecting an empty log.
func runtimeAuditCount(t *testing.T, s *Server) int {
	t.Helper()
	rows, err := s.st.QuerySystemAuditLog(10000)
	if err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	n := 0
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "runtime_") || r.Action == "drain_node" {
			n++
		}
	}
	return n
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
		rows, err := s.st.QuerySystemAuditLog(1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s audit rows: %v %v", action, rows, err)
		}
		if rows[0].Action != "runtime_"+action || rows[0].Target != "solo" {
			t.Errorf("%s audit action/target = %q/%q", action, rows[0].Action, rows[0].Target)
		}
		if rows[0].Details != "Driver: systemd, Identifier: vllm.service" {
			t.Errorf("%s standalone audit detail changed: %q", action, rows[0].Details)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("agent hits = %d, want 3", hits.Load())
	}
}

// Only the exact value acknowledge_replica=true acknowledges; anything else
// is still a 409 with zero agent calls and no audit row.
func TestRuntimeAction_NonTrueAcknowledgeStillRejected(t *testing.T) {
	targets := []struct{ node, action string }{
		{"head", "stop"}, {"head", "restart"}, {"worker-a", "stop"}, {"worker-b", "restart"},
	}
	for _, q := range []string{"acknowledge_replica=false", "acknowledge_replica=1", "acknowledge_replica=yes", "acknowledge_replica=TRUE", "acknowledge_replica="} {
		for _, tg := range targets {
			t.Run(q+"/"+tg.node+"/"+tg.action, func(t *testing.T) {
				s, hits, cleanup := newRuntimeGuardServer(t, true)
				defer cleanup()
				auditBefore := runtimeAuditCount(t, s)
				w := runtimeReq(t, s, tg.node, tg.action, q)
				if w.Code != http.StatusConflict {
					t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
				}
				if msg, _ := decodeBody(t, w)["error"].(string); !strings.Contains(msg, "only the exact value acknowledge_replica=true is accepted") {
					t.Errorf("409 text should say which value is accepted: %q", msg)
				}
				if hits.Load() != 0 {
					t.Errorf("agent hits = %d, want 0", hits.Load())
				}
				if runtimeAuditCount(t, s) != auditBefore {
					t.Errorf("a rejected request must not write a runtime or drain audit row")
				}
			})
		}
	}
}

// An unresolved member is rejected the same way for a non-true value.
func TestRuntimeAction_NonTrueAcknowledgeRejectedForUnresolvedMember(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, false)
	defer cleanup()
	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker-a"],"head":"head"}}`)
	auditBefore := runtimeAuditCount(t, s)
	w := runtimeReq(t, s, "worker-a", "stop", "acknowledge_replica=yes")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 || runtimeAuditCount(t, s) != auditBefore {
		t.Errorf("a rejected request must make no agent call and write no audit row")
	}
}

// An acknowledged stop on a head whose agent fails reports the agent error
// (502) and records no audit row: the audit log only records actions that ran.
func TestRuntimeAction_AcknowledgedAgentFailureNotAudited(t *testing.T) {
	for _, tg := range []struct{ node, action string }{
		{"head", "stop"}, {"head", "restart"}, {"worker-a", "stop"}, {"worker-a", "restart"},
	} {
		t.Run(tg.node+"/"+tg.action, func(t *testing.T) {
			var fail atomic.Bool
			fail.Store(true)
			s, hits, cleanup := newRuntimeGuardServerFailing(t, true, &fail)
			defer cleanup()
			auditBefore := runtimeAuditCount(t, s)
			w := runtimeReq(t, s, tg.node, tg.action, "acknowledge_replica=true")
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502: %s", w.Code, w.Body.String())
			}
			if _, has := decodeBody(t, w)["replica"]; has {
				t.Errorf("an error response carries no replica object: %s", w.Body.String())
			}
			if hits.Load() != 1 {
				t.Errorf("agent hits = %d, want 1", hits.Load())
			}
			if runtimeAuditCount(t, s) != auditBefore {
				t.Errorf("a failed action must not be audited as done")
			}
		})
	}
}

func TestReplicaAuditDetail_QuotesNodeNames(t *testing.T) {
	info := &replicaInfo{Role: "worker", Head: "evil\n, Replica acknowledged: true"}
	got := replicaAuditDetail("Driver: x", info, false)
	if strings.Contains(got, "\n") {
		t.Errorf("a newline in a node name must not reach the audit detail: %q", got)
	}
	if !strings.Contains(got, `Replica head: "evil\n, Replica acknowledged: true"`) {
		t.Errorf("head should be %%q-quoted: %q", got)
	}
}

func TestReplicaWarning_OmitsEmptyMembers(t *testing.T) {
	cases := []struct {
		action replicaAction
		role   router.SchedulingRole
	}{
		{replicaActionStop, router.RoleHead}, {replicaActionRestart, router.RoleWorker}, {replicaActionStop, router.RoleUnresolved},
		{replicaActionDrain, router.RoleHead}, {replicaActionDrain, router.RoleUnresolved},
	}
	for _, c := range cases {
		got := replicaWarning("n", c.action, c.role, &replicaInfo{Role: c.role.String(), Head: "h"})
		if strings.Contains(got, "members: )") || strings.Contains(got, "members: ;") || strings.Contains(got, "(members: ") {
			t.Errorf("%s/%s: empty member list printed: %q", c.action, c.role, got)
		}
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

				auditBefore := runtimeAuditCount(t, s)
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
				if runtimeAuditCount(t, s) != auditBefore {
					t.Errorf("a 409 must not write a runtime audit row")
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
				if rows[0].Action != "runtime_"+action || rows[0].Target != c.node {
					t.Errorf("audit row action/target = %q/%q, want runtime_%s/%s", rows[0].Action, rows[0].Target, action, c.node)
				}
				detail := rows[0].Details
				for _, want := range []string{`Replica role: "` + c.role + `"`, `Replica head: "head"`, "Replica acknowledged: true"} {
					if !strings.Contains(detail, want) {
						t.Errorf("audit detail %q should contain %q", detail, want)
					}
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
	if _, has := decodeBody(t, w)["replica"]; has {
		t.Errorf("start must not carry a replica key: %s", w.Body.String())
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
			rows, err := s.st.QuerySystemAuditLog(1)
			if err != nil || len(rows) != 1 {
				t.Fatalf("audit rows: %v %v", rows, err)
			}
			if rows[0].Action != "drain_node" || rows[0].Target != c.node {
				t.Errorf("audit action/target = %q/%q", rows[0].Action, rows[0].Target)
			}
			for _, want := range []string{`Replica role: "` + c.role + `"`, `Replica head: "head"`} {
				if !strings.Contains(rows[0].Details, want) {
					t.Errorf("drain audit detail %q should contain %q", rows[0].Details, want)
				}
			}
			if strings.Contains(rows[0].Details, "acknowledged") {
				t.Errorf("drain is never acknowledged: %q", rows[0].Details)
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

func TestDrain_UnresolvedMemberWarns(t *testing.T) {
	s, _, cleanup := newRuntimeGuardServer(t, false)
	defer cleanup()
	patchReplicaPeers(t, s, "head", `{"replica_peers":{"members":["head","worker-a"],"head":"head"}}`)
	w := drainReq(t, s, "worker-a")
	if w.Code != http.StatusOK {
		t.Fatalf("drain is never blocked: status %d %s", w.Code, w.Body.String())
	}
	rep, _ := decodeBody(t, w)["replica"].(map[string]any)
	if rep["role"] != "unresolved" {
		t.Errorf("replica = %v, want role unresolved", rep)
	}
	if warn, _ := rep["warning"].(string); !strings.Contains(warn, "cannot be determined") {
		t.Errorf("warning %q should say the role cannot be determined", warn)
	}
	rows, err := s.st.QuerySystemAuditLog(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows: %v %v", rows, err)
	}
	if rows[0].Action != "drain_node" || rows[0].Target != "worker-a" {
		t.Errorf("audit action/target = %q/%q", rows[0].Action, rows[0].Target)
	}
	if !strings.Contains(rows[0].Details, `Replica role: "unresolved"`) || strings.Contains(rows[0].Details, "Replica head") {
		t.Errorf("unresolved drain audit detail = %q", rows[0].Details)
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
	rows, err := s.st.QuerySystemAuditLog(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows: %v %v", rows, err)
	}
	if rows[0].Action != "drain_node" || rows[0].Target != "solo" {
		t.Errorf("audit action/target = %q/%q", rows[0].Action, rows[0].Target)
	}
	if strings.Contains(rows[0].Details, "Replica") || strings.Contains(rows[0].Details, "acknowledged") {
		t.Errorf("standalone drain audit detail must carry no replica text: %q", rows[0].Details)
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
		mk("u1", []string{"u2", "u1"}, "u1"), mk("u2", []string{"u2", "u1"}, "u1"), // declared out of order
	}
	r := router.New(config.RoutingConfig{}, nil, nil)
	roles, heads := r.SchedulingRolesWithHeads(nodes)

	cases := []struct {
		name         string
		action       replicaAction
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
		{name: "u2", action: "stop", role: "worker", head: "u1", members: "u1,u2", warnContains: "breaks that replica"},
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
