package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
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

// newWarmupTestServer builds a Server over nodes named by tags' keys plus
// any extra names in offline. Each node in tags serves /api/tags with the
// given model names (and answers every other path with a trivial 200 so a
// triggered warmup cycle is harmless); each offline node points at a closed
// port, so its catalog can never be read. Backed by a real store because
// the keep-warm handler persists what it accepts.
func newWarmupTestServer(t *testing.T, offline []string, tags map[string][]string) *Server {
	t.Helper()
	var nodes []config.NodeConfig
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		models := tags[name]
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/tags" {
				w.Write([]byte(`{}`))
				return
			}
			type tag struct {
				Name string `json:"name"`
			}
			out := struct {
				Models []tag `json:"models"`
			}{}
			for _, m := range models {
				out.Models = append(out.Models, tag{Name: m})
			}
			_ = json.NewEncoder(w).Encode(out)
		}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, config.NodeConfig{Name: name, URL: srv.URL})
	}
	for _, name := range offline {
		nodes = append(nodes, config.NodeConfig{Name: name, URL: "http://127.0.0.1:1"})
	}
	r := router.New(config.RoutingConfig{}, nodes, nil)
	st, err := store.Open(filepath.Join(t.TempDir(), "warmup.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(r, nil, config.Config{}, st)
}

// declareReplica makes head+workers one valid multi-host replica.
func declareReplica(t *testing.T, s *Server, head string, workers ...string) {
	t.Helper()
	members := append([]string{head}, workers...)
	raw, _ := json.Marshal(map[string]any{"replica_peers": map[string]any{"members": members, "head": head}})
	for _, name := range members {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name, bytes.NewReader(raw))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("declare replica on %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

// declareOneSided makes name declare a replica with other that other never
// declares back, so both resolve as unresolved.
func declareOneSided(t *testing.T, s *Server, name, other string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name, strings.NewReader(`{"replica_peers":{"members":["`+name+`","`+other+`"],"head":"`+name+`"}}`))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("declare one-sided on %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
	}
}

func putNodeWarmup(s *Server, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/admin/nodes/"+name+"/warmup", strings.NewReader(body))
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handleSetNodeWarmup(rec, req)
	return rec
}

func TestWarmupRequestOnlyShrinks(t *testing.T) {
	stored := router.NodeWarmup{Enabled: true, Models: []string{"llama3:latest", "qwen:7b"}}
	tests := []struct {
		name    string
		stored  router.NodeWarmup
		enabled bool
		models  []string
		want    bool
	}{
		{"disable only", stored, false, []string{"llama3:latest", "qwen:7b"}, true},
		{"disable and clear", stored, false, nil, true},
		{"trim", stored, true, []string{"qwen:7b"}, true},
		{"bare name subset of latest", stored, true, []string{"llama3"}, true},
		{"add a model", stored, true, []string{"qwen:7b", "phi3"}, false},
		{"different tag is an add", stored, true, []string{"qwen:14b"}, false},
		{"disabled to enabled flip", router.NodeWarmup{Models: []string{"qwen:7b"}}, true, []string{"qwen:7b"}, false},
		{"empty against no stored config", router.NodeWarmup{}, false, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warmupRequestOnlyShrinks(tt.stored, tt.enabled, tt.models); got != tt.want {
				t.Errorf("warmupRequestOnlyShrinks = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSetNodeWarmup_WorkerRejectsAddButAcceptsClear covers the keep-warm
// ownership rule for a replica worker: nothing can be added or enabled from
// the worker (it is managed on the head), the head's own config is never
// touched by a worker-named request, and a stale worker-keyed config can
// still be disabled, trimmed, or cleared.
func TestSetNodeWarmup_WorkerRejectsAddButAcceptsClear(t *testing.T) {
	s := newWarmupTestServer(t, nil, map[string][]string{
		"head":   {"llama3:latest", "qwen:7b"},
		"worker": {"llama3:latest", "qwen:7b"},
	})
	declareReplica(t, s, "head", "worker")
	s.router.SetNodeWarmup("head", true, []string{"qwen:7b"})
	s.router.SetNodeWarmup("worker", true, []string{"llama3:latest", "qwen:7b"})

	rec := putNodeWarmup(s, "worker", `{"enabled":true,"models":["llama3:latest","qwen:7b","phi3"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("worker add: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `head \"head\"`) {
		t.Errorf("worker add: error should name the head, got %s", rec.Body.String())
	}
	if got := s.router.NodeWarmupSetting("head"); !got.Enabled || len(got.Models) != 1 || got.Models[0] != "qwen:7b" {
		t.Fatalf("head config changed by a worker-named request: %+v", got)
	}

	// Strict subset: accepted, no catalog check needed.
	if rec := putNodeWarmup(s, "worker", `{"enabled":true,"models":["qwen:7b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("worker trim: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Disable, then try to flip back on: the flip counts as an add.
	if rec := putNodeWarmup(s, "worker", `{"enabled":false,"models":["qwen:7b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("worker disable: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := putNodeWarmup(s, "worker", `{"enabled":true,"models":["qwen:7b"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("worker re-enable: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// The clear-only request the UI and CLI send.
	if rec := putNodeWarmup(s, "worker", `{"enabled":false,"models":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("worker clear: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := s.router.NodeWarmupSetting("worker"); got.Enabled || len(got.Models) != 0 {
		t.Errorf("worker config not cleared: %+v", got)
	}
}

func TestSetNodeWarmup_UnresolvedRejected(t *testing.T) {
	s := newWarmupTestServer(t, nil, map[string][]string{"a": {"llama3:latest"}, "b": {"llama3:latest"}})
	declareOneSided(t, s, "a", "b")
	for _, body := range []string{`{"enabled":true,"models":["llama3"]}`, `{"enabled":false,"models":[]}`} {
		if rec := putNodeWarmup(s, "a", body); rec.Code != http.StatusBadRequest {
			t.Errorf("unresolved %s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestSetNodeWarmup_ModelExistence covers catalog validation on a standalone
// node, including the bare-name shorthand and the unreachable-node rules:
// adding fails closed, disabling or trimming still works.
func TestSetNodeWarmup_ModelExistence(t *testing.T) {
	s := newWarmupTestServer(t, []string{"down"}, map[string][]string{"gpu": {"llama3:latest", "qwen:7b"}})

	rec := putNodeWarmup(s, "gpu", `{"enabled":true,"models":["nope:1b"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not available on node") {
		t.Fatalf("unknown model: status = %d, body=%s, want 400 not available", rec.Code, rec.Body.String())
	}
	if rec := putNodeWarmup(s, "gpu", `{"enabled":true,"models":["llama3","qwen:7b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("known models (bare + tagged): status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	s.router.SetNodeWarmup("down", true, []string{"llama3:latest", "qwen:7b"})
	if rec := putNodeWarmup(s, "down", `{"enabled":true,"models":["llama3:latest","qwen:7b","phi3"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("add on unreachable node: status = %d, want 400", rec.Code)
	}
	if rec := putNodeWarmup(s, "down", `{"enabled":true,"models":["qwen:7b"]}`); rec.Code != http.StatusOK {
		t.Fatalf("trim on unreachable node: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := putNodeWarmup(s, "down", `{"enabled":false,"models":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("disable on unreachable node: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSchedules_ReplicaRemapAndModelValidation: a warmup schedule naming a
// worker is stored against its head and validated against the head's
// catalog; drain/undrain/unload against a worker stay on the worker; an
// unresolved node is rejected for every action; an unknown model is
// rejected on create and on a patch that changes the model list.
func TestSchedules_ReplicaRemapAndModelValidation(t *testing.T) {
	// The worker has no readable catalog, so validating against the
	// unremapped node would fail.
	s := newWarmupTestServer(t, []string{"worker"}, map[string][]string{
		"head": {"llama3:latest"},
		"a":    {"llama3:latest"},
		"b":    {"llama3:latest"},
	})
	declareReplica(t, s, "head", "worker")

	rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"warmup","node":"worker","models":["llama3"],"at":"09:00","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("warmup on worker: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var sc router.Schedule
	_ = json.Unmarshal(rec.Body.Bytes(), &sc)
	if sc.Node != "head" {
		t.Errorf("warmup schedule node = %q, want remapped to head", sc.Node)
	}

	for _, action := range []string{"drain", "undrain", "unload"} {
		body := `{"action":"` + action + `","node":"worker","models":["llama3"],"at":"10:00","enabled":true}`
		rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s on worker: status = %d; body=%s", action, rec.Code, rec.Body.String())
		}
		var got router.Schedule
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Node != "worker" {
			t.Errorf("%s schedule node = %q, want worker (never remapped)", action, got.Node)
		}
	}

	rec = doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"warmup","node":"head","models":["missing:1b"],"at":"11:00","enabled":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not available on node") {
		t.Errorf("unknown model: status = %d, body=%s, want 400 not available", rec.Code, rec.Body.String())
	}
	rec = doScheduleRequest(s, http.MethodPatch, "/admin/schedules/"+sc.ID, `{"models":["missing:1b"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("patch to unknown model: status = %d, want 400", rec.Code)
	}

	declareOneSided(t, s, "a", "b")
	for _, action := range []string{"warmup", "drain", "undrain", "unload"} {
		body := `{"action":"` + action + `","node":"a","models":["llama3"],"at":"12:00","enabled":true}`
		if rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s on unresolved node: status = %d, want 400", action, rec.Code)
		}
	}
}

// TestHandleNodes_WarmupSupportedAndWarnings covers the two computed
// keep-warm fields on the node list across all five runtimes.
func TestHandleNodes_WarmupSupportedAndWarnings(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "ollama", URL: "http://o:11434", Runtime: "ollama"},
		{Name: "unknown", URL: "http://u:11434"},
		{Name: "vllm", URL: "http://v:8000", Runtime: "vllm"},
		{Name: "tgi", URL: "http://t:8080", Runtime: "tgi"},
		{Name: "llamacpp", URL: "http://l:8080", Runtime: "llamacpp"},
		{Name: "mlx", URL: "http://m:8080", Runtime: "mlx"},
	}, nil)
	for _, n := range r.Nodes() {
		if n.Name == "ollama" {
			n.Lock()
			n.WarmupWarnings = map[string]string{"llama3": "digest drift"}
			n.Unlock()
		}
	}
	s := NewServer(r, nil, config.Config{})
	rec := httptest.NewRecorder()
	s.handleNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/nodes", nil))
	var out []nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"ollama": true, "unknown": true, "vllm": false, "tgi": false, "llamacpp": false, "mlx": false}
	for _, n := range out {
		if n.WarmupSupported != want[n.Name] {
			t.Errorf("%s: warmupSupported = %v, want %v", n.Name, n.WarmupSupported, want[n.Name])
		}
		if n.Name == "ollama" && n.WarmupWarnings["llama3"] != "digest drift" {
			t.Errorf("ollama: warmupWarnings = %v", n.WarmupWarnings)
		}
	}
	if !strings.Contains(rec.Body.String(), `"warmupSupported":false`) {
		t.Error("warmupSupported must be serialized under its camelCase key")
	}
}
