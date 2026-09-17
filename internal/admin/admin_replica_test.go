package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// TestHandlePatchNode_ReplicaPeersStructuralValidation exercises the PATCH
// handler's structural checks on a declared replica_peers value (non-empty
// members, head present in members, unique members, the declaring node's
// own name present in members) - deliberately not the fleet-wide symmetry
// invariant, which is a read-time property, not something one node's PATCH
// can validate.
func TestHandlePatchNode_ReplicaPeersStructuralValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"head missing", `{"replica_peers":{"members":["a","b"],"head":""}}`, http.StatusBadRequest},
		{"head not in members", `{"replica_peers":{"members":["a","b"],"head":"c"}}`, http.StatusBadRequest},
		{"duplicate members", `{"replica_peers":{"members":["a","a"],"head":"a"}}`, http.StatusBadRequest},
		{"valid declaration", `{"replica_peers":{"members":["a","b"],"head":"a"}}`, http.StatusOK},
		{"explicit clear", `{"replica_peers":{"members":[],"head":""}}`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := router.New(config.RoutingConfig{}, []config.NodeConfig{
				{Name: "a", URL: "http://a:11434", Runtime: "vllm"},
				{Name: "b", URL: "http://b:11434", Runtime: "vllm"},
			}, nil)
			s := NewServer(r, nil, config.Config{})

			req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/a", bytes.NewReader([]byte(tt.body)))
			req.SetPathValue("name", "a")
			rec := httptest.NewRecorder()
			s.handlePatchNode(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHandleNodes_SchedulingRoleFields exercises the admin API's node-list
// response for all four SchedulingRole outcomes: standalone (no
// declaration), head+worker (both sides agree), and unresolved (only one
// side has declared so far - legal and transient, not a PATCH-time error,
// since agreement is checked at read time, not write time).
func TestHandleNodes_SchedulingRoleFields(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "standalone", URL: "http://standalone:11434", Runtime: "vllm"},
		{Name: "head", URL: "http://head:11434", Runtime: "vllm"},
		{Name: "worker", URL: "http://worker:11434", Runtime: "vllm"},
		{Name: "one-sided", URL: "http://one-sided:11434", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	patch := func(name, body string) {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name, bytes.NewReader([]byte(body)))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
	}
	patch("head", `{"replica_peers":{"members":["head","worker"],"head":"head"}}`)
	patch("worker", `{"replica_peers":{"members":["head","worker"],"head":"head"}}`)
	// "one-sided" is named by nobody and declares nothing itself here - the
	// declaration that would make it unresolved is on a DIFFERENT node
	// (below), exercising the "pulled into a component by another node's
	// declaration" case from the fleet closure, not a self-declaration.
	patch("standalone", `{"replica_peers":{"members":["standalone","one-sided"],"head":"standalone"}}`)

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	rec := httptest.NewRecorder()
	s.handleNodes(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var out []nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := make(map[string]nodeResp, len(out))
	for _, n := range out {
		byName[n.Name] = n
	}

	// "standalone" declared replica_peers naming "one-sided", but
	// "one-sided" never declared anything back - the whole component
	// (both names) is unresolved, including "standalone" itself, per the
	// fail-closed union/closure rule: a node's own declaration alone never
	// proves it is safe to schedule.
	if got := byName["standalone"]; got.SchedulingRole != "unresolved" {
		t.Errorf("standalone: SchedulingRole = %q, want unresolved (pulled in by its own one-sided declaration)", got.SchedulingRole)
	}
	if got := byName["one-sided"]; got.SchedulingRole != "unresolved" {
		t.Errorf("one-sided: SchedulingRole = %q, want unresolved", got.SchedulingRole)
	}
	if got := byName["head"]; got.SchedulingRole != "head" || got.ReplicaHead != "head" {
		t.Errorf("head: SchedulingRole = %q, ReplicaHead = %q, want head/head", got.SchedulingRole, got.ReplicaHead)
	}
	if got := byName["worker"]; got.SchedulingRole != "worker" || got.ReplicaHead != "head" {
		t.Errorf("worker: SchedulingRole = %q, ReplicaHead = %q, want worker/head", got.SchedulingRole, got.ReplicaHead)
	}
}

// TestHandleNodes_WorkerAndUnresolvedStillListed confirms the established
// UI/API contract: a worker or unresolved node is a visibility field, never
// a filter - it must still appear in the node list response.
func TestHandleNodes_WorkerAndUnresolvedStillListed(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "head", URL: "http://head:11434", Runtime: "vllm"},
		{Name: "worker", URL: "http://worker:11434", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/worker", bytes.NewReader([]byte(
		`{"replica_peers":{"members":["head","worker"],"head":"head"}}`)))
	req.SetPathValue("name", "worker")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch worker: status = %d, body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	rec2 := httptest.NewRecorder()
	s.handleNodes(rec2, req2)
	var out []nodeResp
	if err := json.Unmarshal(rec2.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d nodes, want 2 (worker must stay listed, never filtered)", len(out))
	}
}
