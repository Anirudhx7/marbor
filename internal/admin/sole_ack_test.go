package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ackFixture is a node a that is the only server of soleModel, with b idle.
func ackFixture(t *testing.T, hits *atomic.Int32) (*Server, func()) {
	t.Helper()
	s, cleanup := newSoleReplicaServer(t, okHandler(hits))
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "")
	return s, cleanup
}

func ackQuery(models ...string) string {
	if len(models) == 0 {
		return "force=true&acknowledge_sole="
	}
	v := url.Values{}
	v.Set("force", "true")
	v["acknowledge_sole"] = models
	return v.Encode()
}

// doAction sends a drain or a restart with a raw query.
func doAction(t *testing.T, s *Server, action, node, query string) *httptest.ResponseRecorder {
	t.Helper()
	if action == "drain" {
		return drainQ(t, s, node, query)
	}
	return runtimeReq(t, s, node, "restart", query)
}

func assertNothingChanged(t *testing.T, s *Server, node string, auditBefore int) {
	t.Helper()
	if isDraining(s, node) {
		t.Errorf("node %s was drained by a refused request", node)
	}
	if n := restartingCount(s); n != 0 {
		t.Errorf("a refused request left %d restart marker(s)", n)
	}
	if after := auditCount(t, s); after != auditBefore {
		t.Errorf("a refused request wrote %d audit row(s)", after-auditBefore)
	}
}

func TestAck_ForceWithMatchingAckProceeds(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			w := doAction(t, s, action, "a", ackQuery(soleModel))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if soleBody(t, w)["forced"] != true {
				t.Errorf("body %s", w.Body.String())
			}
			d := lastAudit(t, s).Details
			if !strings.Contains(d, "Forced: true") || !strings.Contains(d, "Reviewed: true") {
				t.Errorf("audit details %q", d)
			}
			if action == "drain" && !isDraining(s, "a") {
				t.Error("the drain was not applied")
			}
			if action == "restart" && (restartingCount(s) != 1 || hits.Load() != 1) {
				t.Errorf("restart marker %d, agent hits %d", restartingCount(s), hits.Load())
			}
		})
	}
}

func TestAck_BareForceStillProceedsWithoutReviewedMarker(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			if w := doAction(t, s, action, "a", "force=true"); w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			d := lastAudit(t, s).Details
			if !strings.Contains(d, "Forced: true") || strings.Contains(d, "Reviewed") {
				t.Errorf("audit details %q", d)
			}
		})
	}
}

func TestAck_ReviewedMarkerOnlyWhenForcedProceed(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel) // covered: nothing is sole
	if w := drainQ(t, s, "a", ackQuery("")); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if d := lastAudit(t, s).Details; strings.Contains(d, "Reviewed") {
		t.Errorf("nothing was sole, so no marker: %q", d)
	}
}

func TestAck_StaleAckIsRefusedWithDistinctBody(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
			defer cleanup()
			readyNode(t, s, "a", "", soleModel, "org/model-b")
			readyNode(t, s, "b", "")
			before := auditCount(t, s)
			w := doAction(t, s, action, "a", ackQuery(soleModel))
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			body := decodeBody(t, w)
			if body["code"] != "sole_replica" {
				t.Errorf("code %v", body["code"])
			}
			un, _ := body["unacknowledged"].([]any)
			if len(un) != 1 || un[0] != "org/model-b" {
				t.Errorf("unacknowledged = %v", body["unacknowledged"])
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, "org/model-b") || !strings.Contains(msg, "changed since") || strings.Contains(msg, "Pass force=true") {
				t.Errorf("message %q", msg)
			}
			sr := soleBody(t, w)
			if sr["forced"] != false {
				t.Errorf("forced must be false: %v", sr)
			}
			if models, _ := sr["models"].([]any); len(models) != 2 {
				t.Errorf("the current list must be returned: %v", sr)
			}
			assertNothingChanged(t, s, "a", before)
			if hits.Load() != 0 {
				t.Errorf("agent hits = %d", hits.Load())
			}
		})
	}
}

func TestAck_PlainBlockedResponseHasNoUnacknowledgedField(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := ackFixture(t, &hits)
	defer cleanup()
	w := drainQ(t, s, "a", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d", w.Code)
	}
	if _, has := decodeBody(t, w)["unacknowledged"]; has {
		t.Errorf("plain 409 must not carry unacknowledged: %s", w.Body.String())
	}
}

func TestAck_AckWithoutForceNeverProceeds(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			before := auditCount(t, s)
			w := doAction(t, s, action, "a", "acknowledge_sole="+url.QueryEscape(soleModel))
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			assertNothingChanged(t, s, "a", before)
		})
	}
}

func TestAck_ExtraStaleNamesAreIgnored(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := ackFixture(t, &hits)
	defer cleanup()
	if w := drainQ(t, s, "a", ackQuery(soleModel, "org/gone", soleModel)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestAck_EmptyValueCountsAsEmptyList(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			before := auditCount(t, s)
			w := doAction(t, s, action, "a", "force=true&acknowledge_sole=")
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			un, _ := decodeBody(t, w)["unacknowledged"].([]any)
			if len(un) != 1 {
				t.Errorf("unacknowledged = %v", un)
			}
			if msg, _ := decodeBody(t, w)["error"].(string); !strings.Contains(msg, "empty") {
				t.Errorf("message should hint the key was sent empty: %q", msg)
			}
			assertNothingChanged(t, s, "a", before)
		})
	}
}

func TestAck_HostileModelNamesRoundTrip(t *testing.T) {
	names := []string{
		"org/model a,b",
		"mod+el&x=y#z;w%41/q:r",
		"modele-é中文",
		"quote\"and\nnewline",
		strings.Repeat("n", 100),
	}
	for _, name := range names {
		for _, action := range []string{"drain", "restart"} {
			t.Run(action, func(t *testing.T) {
				var hits atomic.Int32
				s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
				defer cleanup()
				readyNode(t, s, "a", "", name)
				readyNode(t, s, "b", "")
				w := doAction(t, s, action, "a", ackQuery(name))
				if w.Code != http.StatusOK {
					t.Fatalf("%q: status %d: %s", name, w.Code, w.Body.String())
				}
				// The ack text never reaches the audit row except as the
				// existing quoted model list.
				d := lastAudit(t, s).Details
				if strings.Count(d, "Reviewed: true") != 1 || strings.Contains(d, "\n") {
					t.Errorf("audit details %q", d)
				}
			})
		}
	}
}

// A node reporting the same model twice holds one model: one ack covers it.
// (A node has a single runtime, so cross-runtime matching by name is covered by
// TestAck_UnacknowledgedMatchesByNameAndDedupes.)
func TestAck_DuplicateLoadedEntriesAreOneModel(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	n := readyNode(t, s, "a", "", soleModel)
	n.Lock()
	n.LoadedModels = append(n.LoadedModels, n.LoadedModels[0])
	n.Unlock()
	readyNode(t, s, "b", "")
	if w := drainQ(t, s, "a", ackQuery(soleModel)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestAck_RuntimeFixtures(t *testing.T) {
	cases := []struct{ runtime, peerRuntime string }{
		{"ollama", "vllm"},
		{"vllm", ""},
		{"llamacpp", ""},
		{"mlx", ""},
	}
	for _, c := range cases {
		for _, action := range []string{"drain", "restart"} {
			t.Run(c.runtime+"/"+action, func(t *testing.T) {
				var hits atomic.Int32
				s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
				defer cleanup()
				readyNode(t, s, "a", c.runtime, "m1", "m2")
				readyNode(t, s, "b", c.peerRuntime)
				before := auditCount(t, s)
				if w := doAction(t, s, action, "a", ackQuery("m1")); w.Code != http.StatusConflict {
					t.Fatalf("partial ack: %d %s", w.Code, w.Body.String())
				}
				assertNothingChanged(t, s, "a", before)
				if w := doAction(t, s, action, "a", ackQuery("m1", "m2")); w.Code != http.StatusOK {
					t.Fatalf("full ack: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

// The sole set grows between the operator's look and the request: refused, and
// nothing is changed.
func TestAck_SetGrowsBetweenReviewAndRequest(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	if w := soleReplicaGET(t, s, "a"); w.Code != http.StatusOK {
		t.Fatalf("GET %d", w.Code)
	}
	// b stops serving it after the review: a is now the only server.
	readyNode(t, s, "b", "")
	before := auditCount(t, s)
	w := drainQ(t, s, "a", ackQuery())
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNothingChanged(t, s, "a", before)
}

func TestAck_MalformedQueryWithSoleModelsIs400(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			before := auditCount(t, s)
			w := doAction(t, s, action, "a", "force=true&acknowledge_sole=%zz")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			assertNothingChanged(t, s, "a", before)
			if hits.Load() != 0 {
				t.Errorf("agent hits = %d", hits.Load())
			}
			// Bare force with a malformed pair fails closed too.
			if w := doAction(t, s, action, "a", "force=true&x=%zz"); w.Code != http.StatusBadRequest {
				t.Errorf("bare force plus malformed pair: %d", w.Code)
			}
		})
	}
}

func TestAck_MalformedQueryWithNothingSoleProceeds(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
			defer cleanup()
			readyNode(t, s, "a", "", soleModel)
			readyNode(t, s, "b", "", soleModel)
			if w := doAction(t, s, action, "a", "force=true&x=%zz"); w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// A stale-ack 409 on the first of two mutual-peer drains leaves the second node
// untouched, and the second request is then decided on its own.
func TestAck_StaleAckOnFirstMutualPeerDrainDoesNotBlockSecond(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel, "org/extra")
	readyNode(t, s, "b", "", soleModel)
	readyNode(t, s, "c", "")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- drainQ(t, s, "a", ackQuery(soleModel)) }()
	select {
	case w := <-done:
		if w.Code != http.StatusConflict {
			t.Fatalf("first drain: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first drain did not finish")
	}
	if isDraining(s, "a") || isDraining(s, "b") || restartingCount(s) != 0 {
		t.Fatal("the stale-ack refusal must change nothing")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- drainQ(t, s, "b", "") }()
	select {
	case w := <-second:
		// a still serves soleModel, so b is not the only server of it.
		if w.Code != http.StatusOK {
			t.Fatalf("second drain: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second drain blocked")
	}
}

func TestAck_ConcurrentAckedDrainsOfMutualPeersExactlyOneWins(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	readyNode(t, s, "c", "")
	reqs := []*http.Request{drainRequest(s, "a", ackQuery()), drainRequest(s, "b", ackQuery())}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, reqs[i])
			codes[i] = w.Code
		}(i)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("drains did not finish")
	}
	// Each request acknowledged an empty list. Whichever runs second sees the
	// first drain, now has a model with no other server it did not
	// acknowledge, and is refused.
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
		t.Fatalf("codes = %v, want one 200 and one 409", codes)
	}
}

// Restart refusal classification.
func TestRestart_RefusalRule(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantMarker  bool
		wantUnclear bool
	}{
		{"422 decodable", 422, `{"ok":false,"error":"no driver"}`, false, false},
		{"400 undecodable", 400, `nope`, false, false},
		{"429 clears", 429, `{"ok":false,"error":"slow down"}`, false, false},
		{"502 decodable ok false", 502, `{"ok":false,"error":"restart failed"}`, true, true},
		{"503 undecodable", 503, `bad gateway html`, true, true},
		{"200 ok false", 200, `{"ok":false,"error":"odd"}`, true, true},
		{"408 keeps", 408, `{"ok":false,"error":"timeout"}`, true, true},
		{"499 keeps", 499, `x`, true, true},
		{"302 keeps", 302, `x`, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, cleanup := newSoleReplicaServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			defer cleanup()
			readyNode(t, s, "a", "", soleModel)
			readyNode(t, s, "b", "", soleModel)
			w := runtimeReq(t, s, "a", "restart", "")
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502: %s", w.Code, w.Body.String())
			}
			if got := restartingCount(s) == 1; got != c.wantMarker {
				t.Errorf("marker kept = %v, want %v", got, c.wantMarker)
			}
			msg, _ := decodeBody(t, w)["error"].(string)
			if unclear := strings.Contains(msg, "outcome unclear"); unclear != c.wantUnclear {
				t.Errorf("unclear wording = %v, want %v: %q", unclear, c.wantUnclear, msg)
			}
			if c.wantUnclear && !strings.Contains(msg, "15m") {
				t.Errorf("wording should name the restart record lifetime: %q", msg)
			}
		})
	}
}

func TestRestart_UnclearWordingOnlyForRestart(t *testing.T) {
	for _, action := range []string{"start", "stop"} {
		t.Run(action, func(t *testing.T) {
			s, cleanup := newSoleReplicaServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte(`{"ok":false,"error":"driver said no"}`))
			})
			defer cleanup()
			readyNode(t, s, "a", "", soleModel)
			readyNode(t, s, "b", "")
			w := runtimeReq(t, s, "a", action, "")
			msg, _ := decodeBody(t, w)["error"].(string)
			if msg != "driver said no" {
				t.Errorf("%s message changed: %q", action, msg)
			}
		})
	}
}

func TestRestart_UnclearWrapsAgentText(t *testing.T) {
	s, cleanup := newSoleReplicaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"ok":false,"error":"systemd: unit failed"}`))
	})
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	w := runtimeReq(t, s, "a", "restart", "")
	msg, _ := decodeBody(t, w)["error"].(string)
	if !strings.Contains(msg, "systemd: unit failed") || !strings.Contains(msg, "outcome unclear") {
		t.Errorf("message %q", msg)
	}
}

// While an unclear restart's marker stands, a drain of the peer is refused;
// a later poll of the restarted node clears it.
func TestRestart_UnclearMarkerBlocksPeerUntilPollClearsIt(t *testing.T) {
	s, cleanup := newSoleReplicaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"ok":false,"error":"restart failed"}`))
	})
	defer cleanup()
	nodeA := readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	if w := runtimeReq(t, s, "a", "restart", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("restart: %d", w.Code)
	}
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusConflict {
		t.Fatalf("peer drain while the marker stands: %d %s", w.Code, w.Body.String())
	}
	nodeA.Lock()
	nodeA.LastPollAt = time.Now().Add(s.router.PollInterval() + time.Second)
	nodeA.Unlock()
	if w := drainQ(t, s, "b", ""); w.Code != http.StatusOK {
		t.Fatalf("peer drain after a later poll: %d %s", w.Code, w.Body.String())
	}
}
