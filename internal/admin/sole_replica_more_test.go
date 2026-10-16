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
)

func auditCount(t *testing.T, s *Server) int {
	t.Helper()
	rows, err := s.st.QuerySystemAuditLog(100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return len(rows)
}

func restartingCount(s *Server) int {
	s.soleReplica.mu.Lock()
	defer s.soleReplica.mu.Unlock()
	return len(s.soleReplica.restarting)
}

// force=true only counts as forced when the node held models nothing else could
// serve: a cold-load-only, unknown-only or covered node reports forced=false.
func TestDrain_ForceDoesNotMarkNonSoleDrainsForced(t *testing.T) {
	var hits atomic.Int32
	t.Run("cold-load only", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "ollama", "llama3")
		readyNode(t, s, "b", "ollama")
		w := drainQ(t, s, "a", "force=true")
		if w.Code != http.StatusOK || soleBody(t, w)["forced"] != false {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if strings.Contains(lastAudit(t, s).Details, "Forced") {
			t.Error("audit must not say Forced")
		}
	})
	t.Run("unknown only", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "", soleModel)
		for _, name := range []string{"b", "c"} {
			n := readyNode(t, s, name, "")
			n.Lock()
			n.LastPollAt = time.Time{}
			n.Unlock()
		}
		w := drainQ(t, s, "a", "force=true")
		sr := soleBody(t, w)
		if w.Code != http.StatusOK || sr["forced"] != false {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if unknown, _ := sr["unknown"].([]any); len(unknown) == 0 {
			t.Errorf("the subtest must yield an unknown item: %s", w.Body.String())
		}
		if models, _ := sr["models"].([]any); len(models) != 0 {
			t.Errorf("unknown must not count as a sole model: %v", models)
		}
	})
	t.Run("covered", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "", soleModel)
		readyNode(t, s, "b", "", soleModel)
		w := drainQ(t, s, "a", "force=true")
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if _, has := decodeBody(t, w)["sole_replica"]; has {
			t.Errorf("a covered drain must not carry sole_replica: %s", w.Body.String())
		}
		if strings.Contains(lastAudit(t, s).Details, "Forced") {
			t.Error("audit must not say Forced")
		}
	})
	t.Run("restart cold-load only", func(t *testing.T) {
		s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
		defer cleanup()
		readyNode(t, s, "a", "ollama", "llama3")
		readyNode(t, s, "b", "ollama")
		w := runtimeReq(t, s, "a", "restart", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		sr := soleBody(t, w)
		if cold, _ := sr["cold_load_only"].([]any); len(cold) != 1 || sr["forced"] != false {
			t.Errorf("sole_replica = %v", sr)
		}
	})
}

// A model another node has pinned but not loaded is not a server of it.
func TestDrain_PinsDoNotCountAsServers(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	s.router.SetPinnedModels("b", []string{soleModel})
	if w := drainQ(t, s, "a", ""); w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
	}
}

// Drains that do not go through this API (scheduled and thermal drains) call the
// router directly and are never refused by the sole-replica check.
func TestRouterDrain_IsNotGuarded(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	if !s.router.DrainNode("a", "scheduled", 0) || !isDraining(s, "a") {
		t.Fatal("a direct router drain must go through")
	}
}

// A drain is held inside its commit step while a drain of the mutual peer is
// sent: the second must wait for the lock and then see the first drain, so it
// is refused. Without the lock it would finish while the first is still held.
func TestDrain_SecondMutualPeerWaitsForCommitAndIsRefused(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	readyNode(t, s, "c", "")

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.soleReplica.afterCommit = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}

	firstDone := make(chan int, 1)
	go func() { firstDone <- drainQ(t, s, "a", "").Code }()
	<-entered

	secondDone := make(chan int, 1)
	go func() { secondDone <- drainQ(t, s, "b", "").Code }()
	select {
	case code := <-secondDone:
		close(release)
		t.Fatalf("second drain finished (%d) while the first was still committing", code)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first drain: %d, want 200", code)
	}
	if code := <-secondDone; code != http.StatusConflict {
		t.Fatalf("second drain: %d, want 409", code)
	}
	if isDraining(s, "b") {
		t.Error("the refused drain must not drain b")
	}
}

func TestRestart_ForceAloneOnReplicaHeadStillNeedsAck(t *testing.T) {
	s, hits, cleanup := newRuntimeGuardServer(t, true)
	defer cleanup()
	readyNode(t, s, "head", "", soleModel)
	for _, n := range []string{"worker-a", "worker-b", "solo"} {
		readyNode(t, s, n, "")
	}
	w := runtimeReq(t, s, "head", "restart", "force=true")
	if w.Code != http.StatusConflict || decodeBody(t, w)["code"] != "replica_member" {
		t.Fatalf("force alone must not satisfy the replica ack: %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Errorf("agent hits = %d", hits.Load())
	}
}

func TestRestart_FailedAgentCallLeavesNoMarker(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s, cleanup := newSoleReplicaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "agent exploded", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"ok\":true}"))
	})
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)

	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", w.Code)
	}
	if n := restartingCount(s); n != 0 {
		t.Fatalf("a failed restart left %d marker(s)", n)
	}
	// The runtime was never taken down, so a still counts as a spare for b.
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusOK {
		t.Fatalf("drain after a failed restart: %d %s", w.Code, w.Body.String())
	}
}

func TestBlockedRequestsWriteNoMarkerAndNoAudit(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	before := auditCount(t, s)

	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusConflict {
		t.Fatalf("restart: %d", w.Code)
	}
	if w := drainQ(t, s, "a", ""); w.Code != http.StatusConflict {
		t.Fatalf("drain: %d", w.Code)
	}
	if n := restartingCount(s); n != 0 {
		t.Errorf("a blocked restart left %d marker(s)", n)
	}
	if after := auditCount(t, s); after != before {
		t.Errorf("blocked attempts wrote %d audit row(s)", after-before)
	}
}

func markerOf(s *Server, name string) restartMarker {
	s.soleReplica.mu.Lock()
	defer s.soleReplica.mu.Unlock()
	return s.soleReplica.restarting[name]
}

// Overlapping restarts of one node share a marker: a refusal of one must not
// delete it while the other is in flight or may have restarted the runtime.
func TestFinishRestart_OverlappingRestartsKeepTheMarker(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	s.soleReplica.mu.Lock()
	s.markRestartStarted("a")
	s.markRestartStarted("a")
	s.soleReplica.mu.Unlock()

	s.finishRestart("a", true) // one refused, one still in flight
	if mk := markerOf(s, "a"); mk.inflight != 1 || !mk.returned.IsZero() {
		t.Fatalf("a refusal beside an in-flight restart changed the marker wrongly: %+v", mk)
	}
	s.finishRestart("a", false) // the other ended with an unknown outcome
	if mk := markerOf(s, "a"); mk.inflight != 0 || mk.returned.IsZero() || !mk.maybeDown {
		t.Fatalf("the surviving restart must stamp returned: %+v", mk)
	}

	// Older restart may have restarted the runtime; a later refusal keeps it.
	s.soleReplica.mu.Lock()
	s.markRestartStarted("a")
	s.soleReplica.mu.Unlock()
	s.finishRestart("a", true)
	if restartingCount(s) != 1 {
		t.Fatal("a refusal must not delete a marker an earlier restart left maybeDown")
	}
}

func TestFinishRestart_LoneRefusalDeletesMarker(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	s.soleReplica.mu.Lock()
	s.markRestartStarted("a")
	s.soleReplica.mu.Unlock()
	s.finishRestart("a", true)
	if restartingCount(s) != 0 {
		t.Fatal("a lone refused restart must delete its marker")
	}
}

// A restart whose outcome is unknown (the agent connection dropped, or a 200
// with an unreadable body) may have restarted the runtime: the marker stays, so
// a drain of the peer is still refused.
func TestRestart_UnknownOutcomeKeepsMarker(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"connection dropped": func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		},
		"unreadable 200": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			s, cleanup := newSoleReplicaServer(t, handler)
			defer cleanup()
			readyNode(t, s, "a", "", soleModel)
			readyNode(t, s, "b", "", soleModel)
			if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusBadGateway {
				t.Fatalf("restart: %d, want 502", w.Code)
			}
			if mk := markerOf(s, "a"); mk.returned.IsZero() || !mk.maybeDown {
				t.Fatalf("an unknown outcome must keep a stamped marker: %+v", mk)
			}
			if w := drainQ(t, s, "b", ""); w.Code != http.StatusConflict {
				t.Fatalf("drain of the peer: %d, want 409: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRestart_SuccessStampsReturned(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusOK {
		t.Fatalf("restart: %d", w.Code)
	}
	mk := markerOf(s, "a")
	if mk.returned.IsZero() || mk.inflight != 0 {
		t.Fatalf("a successful restart must stamp returned: %+v", mk)
	}
}

func TestSoleCheck_ZeroValueIsNotNodeMissing(t *testing.T) {
	var c soleCheck
	if !c.found() || c.blocked() || c.forced() {
		t.Fatalf("zero soleCheck = found %v blocked %v forced %v", c.found(), c.blocked(), c.forced())
	}
}

func TestDrain_BadBodyAndGraceAreRejectedBeforeSoleCheck(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	for _, tc := range []struct{ node, body string }{
		{"a", "{bad"},
		{"a", "{\"grace_period_seconds\":-1}"},
		{"nope", "{bad"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/nodes/"+tc.node+"/drain", strings.NewReader(tc.body))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status %d, want 400", tc.node, tc.body, w.Code)
		}
	}
	if isDraining(s, "a") {
		t.Error("a rejected request must not drain")
	}
}

func TestSoleReplicaGET_ReflectsInFlightRestart(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"ok\":true}"))
	}
	s, cleanup := newSoleReplicaServer(t, handler)
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)

	w := soleReplicaGET(t, s, "b")
	if models, _ := soleBody(t, w)["models"].([]any); len(models) != 0 {
		t.Fatalf("before the restart b is covered by a: %v", models)
	}
	done := make(chan int, 1)
	go func() { done <- runtimeReq(t, s, "a", "restart", "").Code }()
	<-started
	w = soleReplicaGET(t, s, "b")
	if models, _ := soleBody(t, w)["models"].([]any); len(models) != 1 {
		t.Errorf("while a restarts b is the only server: %v", soleBody(t, w))
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("restart: %d", code)
	}
}

func TestSoleReplicaMessage_RestartWording(t *testing.T) {
	msg := soleReplicaMessage("n", soleActionRestart, []router.SoleReplicaModel{{Model: "m1"}})
	if !strings.Contains(msg, "restarting the runtime on node") {
		t.Errorf("message = %q", msg)
	}
}
