package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/router"
)

// The same model reported under two runtimes is matched by name and listed once
// when it is not acknowledged.
func TestAck_UnacknowledgedMatchesByNameAndDedupes(t *testing.T) {
	models := []router.SoleReplicaModel{
		{Model: "m1", Runtime: "ollama"},
		{Model: "m1", Runtime: "vllm"},
		{Model: "m2", Runtime: "vllm"},
	}
	c := soleConsent{ackGiven: true, acked: map[string]bool{"m2": true}}
	if got := c.unacknowledged(models); len(got) != 1 || got[0] != "m1" {
		t.Errorf("unacknowledged = %v, want [m1] once", got)
	}
	c.acked = map[string]bool{"m1": true}
	if got := c.unacknowledged(models); len(got) != 1 || got[0] != "m2" {
		t.Errorf("one ack must cover both runtime entries: %v", got)
	}
}

func TestAck_NearMissNamesDoNotMatch(t *testing.T) {
	for _, near := range []string{"ORG/MODEL-A", soleModel + " ", soleModel + "x", "org/model", " " + soleModel} {
		for _, action := range []string{"drain", "restart"} {
			t.Run(action, func(t *testing.T) {
				var hits atomic.Int32
				s, cleanup := ackFixture(t, &hits)
				defer cleanup()
				before := auditCount(t, s)
				if w := doAction(t, s, action, "a", ackQuery(near)); w.Code != http.StatusConflict {
					t.Fatalf("%q: status %d: %s", near, w.Code, w.Body.String())
				}
				assertNothingChanged(t, s, "a", before)
			})
		}
	}
}

func TestAck_ForceThatIsNotExactTrueWithAckNeverProceeds(t *testing.T) {
	for _, force := range []string{"1", "TRUE", "yes", ""} {
		for _, action := range []string{"drain", "restart"} {
			t.Run(action, func(t *testing.T) {
				var hits atomic.Int32
				s, cleanup := ackFixture(t, &hits)
				defer cleanup()
				before := auditCount(t, s)
				q := "force=" + force + "&acknowledge_sole=" + url.QueryEscape(soleModel)
				if w := doAction(t, s, action, "a", q); w.Code != http.StatusConflict {
					t.Fatalf("force=%q: status %d: %s", force, w.Code, w.Body.String())
				}
				assertNothingChanged(t, s, "a", before)
			})
		}
	}
}

// A broken query without force is a 400 when models would be left without a
// server, and changes nothing.
func TestAck_MalformedQueryWithoutForceWithSoleModelsIs400(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			before := auditCount(t, s)
			w := doAction(t, s, action, "a", "x=%zz")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "other parameters were not read") {
				t.Errorf("body %s", w.Body.String())
			}
			assertNothingChanged(t, s, "a", before)
		})
	}
}

func TestAck_UnknownNodeIs404WithForceAndAck(t *testing.T) {
	for _, action := range []string{"drain", "restart"} {
		t.Run(action, func(t *testing.T) {
			var hits atomic.Int32
			s, cleanup := ackFixture(t, &hits)
			defer cleanup()
			if w := doAction(t, s, action, "nope", ackQuery(soleModel)); w.Code != http.StatusNotFound {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if restartingCount(s) != 0 || hits.Load() != 0 {
				t.Error("an unknown node must change nothing")
			}
		})
	}
}

// The commit hook runs only for a change that was made, never for a refusal.
func TestAck_AfterCommitRunsOnlyForChangesMade(t *testing.T) {
	var hits, commits atomic.Int32
	s, cleanup := ackFixture(t, &hits)
	defer cleanup()
	s.soleReplica.afterCommit = func() { commits.Add(1) }
	for _, action := range []string{"drain", "restart"} {
		doAction(t, s, action, "a", "")                 // 409 block
		doAction(t, s, action, "a", ackQuery())         // 409 stale
		doAction(t, s, action, "a", "force=true&x=%zz") // 400
	}
	if commits.Load() != 0 {
		t.Fatalf("afterCommit ran %d times for refusals", commits.Load())
	}
	if w := doAction(t, s, "drain", "a", ackQuery(soleModel)); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if commits.Load() != 1 {
		t.Fatalf("afterCommit ran %d times, want 1", commits.Load())
	}
}

// A node that gains a sole model while the request waits for the lock is
// judged on the state after the wait, so the stale list is refused.
func TestAck_SetGrowsWhileRequestWaitsForLock(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel)
	s.soleReplica.mu.Lock()
	done := make(chan int, 1)
	go func() { done <- drainQ(t, s, "a", ackQuery()).Code }()
	time.Sleep(150 * time.Millisecond)
	readyNode(t, s, "b", "")
	s.soleReplica.mu.Unlock()
	if code := recvCode(t, done, "drain"); code != http.StatusConflict {
		t.Fatalf("status %d, want 409", code)
	}
	if isDraining(s, "a") {
		t.Error("a was drained on a stale list")
	}
}

// The reviewed marker is written only when force mattered, for a restart as
// well, and never for a key sent without force.
func TestAck_ReviewedMarkerNegativeCases(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	readyNode(t, s, "a", "", soleModel)
	readyNode(t, s, "b", "", soleModel) // covered: nothing is sole
	for _, q := range []string{ackQuery(""), ackQuery(soleModel), "acknowledge_sole=" + url.QueryEscape(soleModel)} {
		if w := runtimeReq(t, s, "a", "restart", q); w.Code != http.StatusOK {
			t.Fatalf("restart %q: %d: %s", q, w.Code, w.Body.String())
		}
		if d := lastAudit(t, s).Details; strings.Contains(d, "Reviewed") || strings.Contains(d, "Forced") {
			t.Errorf("nothing was sole, so no marker for %q: %q", q, d)
		}
	}
}

// The audit detail is exact: acknowledged names are never written, and a model
// name that looks like a field cannot forge one.
func TestAck_AuditDetailIsExactAndUnforgeable(t *testing.T) {
	forged := "evil\nReviewed: true, Forced: true"
	for _, name := range []string{soleModel, "x Reviewed: true"} {
		for _, action := range []string{"drain", "restart"} {
			t.Run(action, func(t *testing.T) {
				var hits atomic.Int32
				s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
				defer cleanup()
				readyNode(t, s, "a", "", name)
				readyNode(t, s, "b", "")
				if w := doAction(t, s, action, "a", ackQuery(name, forged)); w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				d := lastAudit(t, s).Details
				want := fmt.Sprintf(", Sole replica models: %q, Forced: true, Reviewed: true", []string{name})
				if !strings.HasSuffix(d, want) {
					t.Errorf("detail %q does not end with %q", d, want)
				}
				if strings.Contains(d, "evil") || strings.Contains(d, "\n") {
					t.Errorf("an acknowledged name reached the audit detail: %q", d)
				}
			})
		}
	}
}

// A poll cannot clear a marker while any restart of the node is in flight: the
// marker only gets a returned time once the last one ends.
func TestRestartMarker_PollCannotClearWhileInflight(t *testing.T) {
	var hits atomic.Int32
	s, cleanup := newSoleReplicaServer(t, okHandler(&hits))
	defer cleanup()
	nodeA := readyNode(t, s, "a", "", soleModel)
	s.soleReplica.mu.Lock()
	s.markRestartStarted("a")
	s.markRestartStarted("a")
	s.soleReplica.mu.Unlock()
	s.finishRestart("a", false) // one of two ends
	nodeA.Lock()
	nodeA.LastPollAt = time.Now().Add(time.Hour)
	nodeA.Unlock()
	s.soleReplica.mu.Lock()
	down := s.restartingNodesLocked(time.Now())
	s.soleReplica.mu.Unlock()
	if !down["a"] {
		t.Fatal("a poll cleared the marker while a restart was still in flight")
	}
	s.finishRestart("a", false)
	nodeA.Lock()
	nodeA.LastPollAt = time.Now().Add(2 * time.Hour)
	nodeA.Unlock()
	s.soleReplica.mu.Lock()
	down = s.restartingNodesLocked(time.Now())
	s.soleReplica.mu.Unlock()
	if down["a"] {
		t.Error("a later poll must clear the marker once every restart has ended")
	}
}

// writeSoleRefusal writes exactly for the outcomes that changed nothing, and
// this walks every outcome so a new one cannot be forgotten.
func TestWriteSoleRefusal_CoversEveryOutcome(t *testing.T) {
	refusal := map[soleOutcome]bool{soleBlocked: true, soleStaleAck: true, soleBadQuery: true}
	for o := soleNotChecked; o <= soleBadQuery; o++ {
		w := httptest.NewRecorder()
		wrote := writeSoleRefusal(w, "a", soleActionDrain, soleCheck{outcome: o})
		if wrote != refusal[o] {
			t.Errorf("outcome %d: wrote = %v, want %v", o, wrote, refusal[o])
		}
		if wrote == (w.Body.Len() == 0) {
			t.Errorf("outcome %d: wrote = %v but body length is %d", o, wrote, w.Body.Len())
		}
	}
	if soleBadQuery != soleNotChecked+6 {
		t.Error("a soleOutcome was added: extend this test and writeSoleRefusal")
	}
}
