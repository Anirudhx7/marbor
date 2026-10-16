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
		if w.Code != http.StatusOK || soleBody(t, w)["forced"] != false {
			t.Fatalf("%d %s", w.Code, w.Body.String())
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

func TestDrain_ConcurrentMutualPeersRepeatedly(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	for round := 0; round < 40; round++ {
		readyNode(t, s, "a", "", soleModel)
		readyNode(t, s, "b", "", soleModel)
		readyNode(t, s, "c", "")
		s.router.UndrainNode("a")
		s.router.UndrainNode("b")

		start := make(chan struct{})
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, name := range []string{"a", "b"} {
			req := drainRequest(s, name, "")
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, req)
				codes[i] = w.Code
			}(i)
		}
		close(start)
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
			t.Fatalf("round %d: codes = %v, want exactly one 200 and one 409", round, codes)
		}
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

// An overlapping restart of the same node owns the marker: the first restart
// finishing must not stamp or delete the newer marker.
func TestFinishRestart_OnlyTouchesItsOwnMarker(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	s.soleReplica.mu.Lock()
	first := s.markRestartStarted("a")
	second := s.markRestartStarted("a")
	s.soleReplica.mu.Unlock()
	if first == second {
		t.Fatal("tokens must differ")
	}

	s.finishRestart("a", first, true)
	s.soleReplica.mu.Lock()
	mk := s.soleReplica.restarting["a"]
	s.soleReplica.mu.Unlock()
	if mk.token != second || !mk.returned.IsZero() {
		t.Fatalf("first restart changed the second marker: %+v", mk)
	}
	s.finishRestart("a", first, false)
	if restartingCount(s) != 1 {
		t.Fatal("a stale failure must not delete the newer marker")
	}
	s.finishRestart("a", second, false)
	if restartingCount(s) != 0 {
		t.Fatal("the owning restart failing must delete its marker")
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
