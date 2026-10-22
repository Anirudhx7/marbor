package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const soleModel = "org/model-a"

// newSoleReplicaServer builds standalone vllm nodes a, b, c, each with a mock
// agent running handler, runtime control accepted, and an in-memory store.
func newSoleReplicaServer(t *testing.T, handler http.HandlerFunc) (*Server, func()) {
	t.Helper()
	var nodes []replicaDeleteTestNode
	for _, n := range []string{"a", "b", "c"} {
		nodes = append(nodes, replicaDeleteTestNode{name: n, host: "host-" + n, handler: handler})
	}
	s, cleanup := newReplicaDeleteTestServer(t, nodes)
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
	// Every node starts healthy and freshly polled, holding nothing, so a test
	// only describes what differs.
	for _, n := range []string{"a", "b", "c"} {
		readyNode(t, s, n, "")
	}
	return s, cleanup
}

func okHandler(hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
}

// readyNode marks a node healthy and freshly polled, holding models under the
// given runtime (empty keeps vllm).
func readyNode(t *testing.T, s *Server, name, runtime string, models ...string) *router.NodeState {
	t.Helper()
	for _, n := range s.router.Nodes() {
		if n.Name != name {
			continue
		}
		n.Lock()
		defer n.Unlock()
		n.Healthy = true
		n.LastPollAt = time.Now()
		if runtime != "" {
			n.Runtime = runtime
		}
		n.LoadedModels = nil
		for _, m := range models {
			n.LoadedModels = append(n.LoadedModels, router.ModelInfo{Name: m})
		}
		return n
	}
	t.Fatalf("node %q not found", name)
	return nil
}

func drainRequest(s *Server, node, query string) *http.Request {
	target := "/admin/nodes/" + node + "/drain"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	return req
}

func drainQ(t *testing.T, s *Server, node, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, drainRequest(s, node, query))
	return w
}

func soleReplicaGET(t *testing.T, s *Server, node string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/"+node+"/sole-replica", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func isDraining(s *Server, name string) bool {
	for _, n := range s.router.Nodes() {
		if n.Name == name {
			n.RLock()
			defer n.RUnlock()
			return n.Draining
		}
	}
	return false
}

func soleBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	sr, ok := decodeBody(t, w)["sole_replica"].(map[string]any)
	if !ok {
		t.Fatalf("no sole_replica object in %s", w.Body.String())
	}
	return sr
}

func lastAudit(t *testing.T, s *Server) store.SystemAuditEntry {
	t.Helper()
	rows, err := s.st.QuerySystemAuditLog(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows: %v %v", rows, err)
	}
	return rows[0]
}

func TestDrain_SoleReplicaBlockedWithoutForce(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")

	w := drainQ(t, s, "a", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["code"] != "sole_replica" {
		t.Errorf("code = %v", body["code"])
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "force=true") || !strings.Contains(msg, soleModel) {
		t.Errorf("error text = %q", msg)
	}
	sr := soleBody(t, w)
	if sr["forced"] != false {
		t.Errorf("forced = %v, want false", sr["forced"])
	}
	models, _ := sr["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["model"] != soleModel || models[0].(map[string]any)["runtime"] != "vllm" {
		t.Errorf("models = %v", sr["models"])
	}
	if sr["cold_load_only"] == nil || sr["unknown"] == nil {
		t.Errorf("arrays must be present, got %v", sr)
	}
	if isDraining(s, "a") {
		t.Error("a blocked drain must not change state")
	}
}

func TestDrain_ForceProceedsAndIsAudited(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")

	w := drainQ(t, s, "a", "force=true")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if soleBody(t, w)["forced"] != true {
		t.Errorf("forced should be true: %s", w.Body.String())
	}
	if !isDraining(s, "a") {
		t.Error("node should be draining")
	}
	row := lastAudit(t, s)
	if row.Action != "drain_node" || !strings.Contains(row.Details, `Sole replica models: ["`+soleModel+`"]`) || !strings.Contains(row.Details, "Forced: true") {
		t.Errorf("audit = %+v", row)
	}
}

func TestDrain_ForceValueIsExact(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	for _, v := range []string{"force=1", "force=yes", "force=TRUE", "force=", "force=false"} {
		if w := drainQ(t, s, "a", v); w.Code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409", v, w.Code)
		}
	}
}

func TestDrain_CoveredNodeIsUnchanged(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)

	w := drainQ(t, s, "a", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if _, has := decodeBody(t, w)["sole_replica"]; has {
		t.Errorf("a covered drain must not carry sole_replica: %s", w.Body.String())
	}
	if strings.Contains(lastAudit(t, s).Details, "Sole replica") {
		t.Error("audit detail must not mention sole replicas")
	}
}

func TestDrain_ColdOnlyAndUnknownProceedWithLists(t *testing.T) {
	var hits atomic.Int32
	t.Run("cold-load only", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "ollama", "llama3")
		readyNode(t, s, "b", "ollama")
		w := drainQ(t, s, "a", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		sr := soleBody(t, w)
		if cold, _ := sr["cold_load_only"].([]any); len(cold) != 1 {
			t.Errorf("cold_load_only = %v", sr["cold_load_only"])
		}
		if sr["forced"] != false {
			t.Errorf("forced = %v, want false", sr["forced"])
		}
	})
	t.Run("unknown only", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "", soleModel)
		// b and c have never been polled, so what they hold cannot be verified.
		for _, name := range []string{"b", "c"} {
			n := readyNode(t, s, name, "")
			n.Lock()
			n.LastPollAt = time.Time{}
			n.Unlock()
		}
		w := drainQ(t, s, "a", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		sr := soleBody(t, w)
		if unk, _ := sr["unknown"].([]any); len(unk) == 0 {
			t.Errorf("unknown = %v", sr["unknown"])
		}
		if sr["forced"] != false {
			t.Errorf("forced = %v, want false", sr["forced"])
		}
	})
}

func TestDrain_UnknownNode404(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	if w := drainQ(t, s, "nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

func TestRestart_SoleReplicaBlockedAndForced(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")

	w := runtimeReq(t, s, "a", "restart", "")
	if w.Code != http.StatusConflict || decodeBody(t, w)["code"] != "sole_replica" {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("a blocked restart must not reach the agent, hits=%d", hits.Load())
	}
	for _, v := range []string{"force=1", "force=yes", "force=TRUE"} {
		if w := runtimeReq(t, s, "a", "restart", v); w.Code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409", v, w.Code)
		}
	}
	w = runtimeReq(t, s, "a", "restart", "force=true")
	if w.Code != http.StatusOK {
		t.Fatalf("forced: status %d: %s", w.Code, w.Body.String())
	}
	if hits.Load() != 1 || soleBody(t, w)["forced"] != true {
		t.Errorf("hits=%d body=%s", hits.Load(), w.Body.String())
	}
	row := lastAudit(t, s)
	if row.Action != "runtime_restart" || !strings.Contains(row.Details, "Forced: true") || !strings.Contains(row.Details, soleModel) {
		t.Errorf("audit = %+v", row)
	}
}

func TestRestart_StopStartAndAlreadyDrainingNotGuarded(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	for _, action := range []string{"stop", "start"} {
		if w := runtimeReq(t, s, "a", action, ""); w.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", action, w.Code, w.Body.String())
		}
	}
	// Forced drain, then restart of the now-draining node: no second 409.
	if w := drainQ(t, s, "a", "force=true"); w.Code != http.StatusOK {
		t.Fatalf("drain: %d %s", w.Code, w.Body.String())
	}
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusOK {
		t.Fatalf("restart of a draining node: %d %s", w.Code, w.Body.String())
	}
	// Undrain is unchanged.
	req := httptest.NewRequest(http.MethodDelete, "/admin/nodes/a/drain", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || isDraining(s, "a") {
		t.Errorf("undrain: %d draining=%v", rec.Code, isDraining(s, "a"))
	}
}

func TestRestart_PrecedingChecksComeFirst(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")

	n := readyNode(t, s, "a", "", soleModel)
	n.Lock()
	n.AgentPresent = false
	n.Unlock()
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unreachable agent: %d, want 503", w.Code)
	}
	n.Lock()
	n.AgentPresent = true
	n.AgentCapabilities = []string{"status"}
	n.Unlock()
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusNotImplemented {
		t.Errorf("no capability: %d, want 501", w.Code)
	}
	n.Lock()
	n.AgentCapabilities = []string{"status", "runtime.restart"}
	n.Unlock()
	s.router.SetNodeControl("a", router.ControlConfig{})
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("no driver: %d, want 422", w.Code)
	}
}

func TestRestart_ReplicaMemberAckComesBeforeSoleCheck(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	for _, n := range []string{"head", "worker-a", "worker-b", "solo"} {
		runtime := ""
		if n == "head" {
			readyNode(t, s, n, runtime, soleModel)
			continue
		}
		readyNode(t, s, n, runtime)
	}
	w := runtimeReq(t, s, "head", "restart", "")
	if w.Code != http.StatusConflict || decodeBody(t, w)["code"] != "replica_member" {
		t.Fatalf("want replica_member 409, got %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Errorf("agent hits = %d", hits.Load())
	}
	w = runtimeReq(t, s, "head", "restart", "acknowledge_replica=true")
	if w.Code != http.StatusConflict || decodeBody(t, w)["code"] != "sole_replica" {
		t.Fatalf("after ack the sole check applies: %d %s", w.Code, w.Body.String())
	}
	w = runtimeReq(t, s, "head", "restart", "acknowledge_replica=true&force=true")
	if w.Code != http.StatusOK {
		t.Fatalf("both acks: %d %s", w.Code, w.Body.String())
	}
}

func TestDrain_MutualPeersExactlyOneSucceeds(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	readyNode(t, s, "c", "")

	// drainRequest calls AdminToken, which sets the server's demo-auth flag, so
	// build both requests before any handler goroutine reads that flag.
	reqs := []*http.Request{drainRequest(s, "a", ""), drainRequest(s, "b", "")}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range reqs {
		req := reqs[i]
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("codes = %v, want exactly one 200 and one 409", codes)
	}
}

func TestRestartMarker_BlocksPeerDrainWhileInFlightAndAfterReturn(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}
	s, cleanup := newSoleReplicaServer(t, handler)
	defer cleanup()
	nodeA := readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	readyNode(t, s, "c", "")

	done := make(chan int, 1)
	go func() { done <- runtimeReq(t, s, "a", "restart", "").Code }()
	<-started

	// a is restarting, so b is the only server left for the model.
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusConflict {
		t.Fatalf("drain during restart: %d %s", w.Code, w.Body.String())
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("restart: %d", code)
	}

	// The restart returned but no later poll has confirmed a is back.
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusConflict {
		t.Fatalf("drain right after restart: %d %s", w.Code, w.Body.String())
	}
	// A poll that lands within one interval of the return does not clear it.
	nodeA.Lock()
	nodeA.LastPollAt = time.Now()
	nodeA.Unlock()
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusConflict {
		t.Fatalf("drain with an in-flight poll: %d %s", w.Code, w.Body.String())
	}
	// A poll a full interval later confirms a is serving again.
	nodeA.Lock()
	nodeA.LastPollAt = time.Now().Add(s.router.PollInterval() + time.Second)
	nodeA.Unlock()
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusOK {
		t.Fatalf("drain after confirmation: %d %s", w.Code, w.Body.String())
	}
}

func TestRestartMarker_ExpiresAfterTTL(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	s.soleReplica.mu.Lock()
	s.soleReplica.restarting = map[string]restartMarker{"a": {started: time.Now().Add(-restartMarkerTTL - time.Minute)}}
	s.soleReplica.mu.Unlock()
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusOK {
		t.Fatalf("an expired marker must not count: %d %s", w.Code, w.Body.String())
	}
}

func TestSoleReplicaGET(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")

	w := soleReplicaGET(t, s, "a")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["node"] != "a" {
		t.Errorf("node = %v", body["node"])
	}
	sr := soleBody(t, w)
	if models, _ := sr["models"].([]any); len(models) != 1 {
		t.Errorf("models = %v", sr["models"])
	}
	if sr["cold_load_only"] == nil || sr["unknown"] == nil {
		t.Errorf("arrays must be non-null: %v", sr)
	}
	if _, has := sr["forced"]; has {
		t.Errorf("the read-only check has no forced field: %v", sr)
	}
	if isDraining(s, "a") || len(s.soleReplica.restarting) != 0 {
		t.Error("GET must not change state")
	}
	// A node with nothing to report still returns all three arrays.
	w = soleReplicaGET(t, s, "b")
	sr = soleBody(t, w)
	for _, k := range []string{"models", "cold_load_only", "unknown"} {
		if arr, ok := sr[k].([]any); !ok || arr == nil {
			t.Errorf("%s must be an empty array, got %v", k, sr[k])
		}
	}
	if w := soleReplicaGET(t, s, "nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown node: %d", w.Code)
	}
}

func TestSoleReplicaGET_RequiresAuth(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/a/sole-replica", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 401/403", w.Code)
	}
}

func TestSoleReplicaMessage_TruncatesLongLists(t *testing.T) {
	var models []router.SoleReplicaModel
	for _, m := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"} {
		models = append(models, router.SoleReplicaModel{Model: m, Runtime: "vllm"})
	}
	msg := soleReplicaMessage("n", "drain", models)
	if !strings.Contains(msg, "and 2 more") || strings.Contains(msg, "m6") {
		t.Errorf("message = %q", msg)
	}
}
