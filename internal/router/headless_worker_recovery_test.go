package router

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// switchFleet is head + worker (a replica group), solo (standalone) and orphan
// (an unresolved declaration), all probed over HTTP against one server that
// can be switched between failing and answering.
type switchFleet struct {
	r    *Router
	hook *hookCollector
	down atomic.Bool
}

func newSwitchFleet(t *testing.T) *switchFleet {
	t.Helper()
	f := &switchFleet{hook: newHookCollector(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	f.r = newTestRouter([]config.NodeConfig{
		{Name: "head", URL: srv.URL, Runtime: "ollama"},
		{Name: "worker", URL: srv.URL, Runtime: "ollama"},
		{Name: "solo", URL: srv.URL, Runtime: "ollama"},
		{Name: "orphan", URL: srv.URL, Runtime: "ollama"},
	}, config.WebhookConfig{Enabled: true, URL: f.hook.srv.URL})
	declareGroup(f.r, "head", "worker")
	orphan := nodeByName(t, f.r, "orphan")
	orphan.mu.Lock()
	orphan.ReplicaPeers = &store.ReplicaPeers{Members: []string{"orphan", "ghost"}, Head: "orphan"}
	orphan.mu.Unlock()
	return f
}

func (f *switchFleet) poll(t *testing.T, name string, times int) {
	t.Helper()
	n := nodeByName(t, f.r, name)
	for i := 0; i < times; i++ {
		f.r.pollNode(n)
	}
}

func (f *switchFleet) healthy(t *testing.T, name string) bool {
	t.Helper()
	n := nodeByName(t, f.r, name)
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.Healthy
}

// Standalone, head and unresolved nodes announce a node_down once the failure
// threshold is crossed and a node_up once the success threshold brings them
// back: the worker muting must not change this.
func TestPollNode_NonWorkersAnnounceDownThenUp(t *testing.T) {
	for _, name := range []string{"solo", "head", "orphan"} {
		t.Run(name, func(t *testing.T) {
			f := newSwitchFleet(t)
			f.down.Store(true)
			f.poll(t, name, 3)
			if got := f.hook.waitFor(1); len(got) != 1 || got[0] != "node_down:"+name {
				t.Fatalf("events = %v, want node_down:%s", got, name)
			}
			f.down.Store(false)
			f.poll(t, name, 2)
			got := f.hook.waitFor(2)
			if len(got) != 2 || got[1] != "node_up:"+name {
				t.Fatalf("events = %v, want node_down then node_up for %s", got, name)
			}
		})
	}
}

// A node that went down as a standalone node (node_down sent) and is declared
// a worker mid-outage stays silent while down and still sends node_up when it
// recovers, so a receiver never keeps a stale down.
func TestMarkFailure_DownThenDeclaredWorkerRecoveryStillSendsNodeUp(t *testing.T) {
	f := newSwitchFleet(t)
	clearGroup(f.r)
	f.down.Store(true)
	f.poll(t, "worker", 3) // standalone: node_down sent
	if got := f.hook.waitFor(1); len(got) != 1 || got[0] != "node_down:worker" {
		t.Fatalf("events = %v, want node_down:worker", got)
	}
	declareGroup(f.r, "head", "worker") // becomes a worker mid-outage
	if v, _ := prevHealthyOf(f.r, "worker"); v {
		t.Fatal("prevHealthy must remain false after node_down was sent")
	}
	// Still down as a worker: no further event. This is a weak check (an
	// already-unhealthy node has no transition to announce); the recovery
	// node_up below is the assertion that matters.
	f.poll(t, "worker", 2)
	if got := f.hook.settleWithSentinel(t, f.r); len(got) != 1 {
		t.Fatalf("events = %v, want just the original node_down", got)
	}
	f.down.Store(false)
	f.poll(t, "worker", 2)
	got := f.hook.waitForEvent("node_up:worker")
	if countEvent(got, "node_up:worker") != 1 {
		t.Fatalf("events = %v, want node_up:worker on recovery", got)
	}
}

// A worker that fails while declared a worker (silent), is then declared
// standalone while still failing and recovers sends no node_down and no node_up
// for that outage.
func TestMarkFailure_WorkerRedeclaredStandaloneRecoversSilently(t *testing.T) {
	f := newSwitchFleet(t)
	f.down.Store(true)
	f.poll(t, "worker", 3)
	clearGroup(f.r)
	f.poll(t, "worker", 2)
	f.down.Store(false)
	f.poll(t, "worker", 2)
	if !f.healthy(t, "worker") {
		t.Fatal("worker should have recovered")
	}
	if got := f.hook.settleWithSentinel(t, f.r); len(got) != 0 {
		t.Fatalf("events = %v, want none for a muted outage", got)
	}
}

// While probes fail but the node is still inside the success hysteresis, a
// worker stays presented as a headless worker and sends nothing. A node that is
// still Healthy with one failure is muted too, and none of it touches
// prevHealthy.
func TestPollNode_WorkerHysteresisWindowStaysHeadlessAndSilent(t *testing.T) {
	f := newSwitchFleet(t)
	headless := func() bool {
		// Resolve the role before taking the node lock: resolution locks every node.
		role := f.r.roleForWebhook("worker")
		n := nodeByName(t, f.r, "worker")
		n.mu.RLock()
		defer n.mu.RUnlock()
		return IsHeadlessWorker(role, n.Healthy, n.Failures)
	}
	f.down.Store(true)
	f.poll(t, "worker", 3)
	f.down.Store(false)
	f.poll(t, "worker", 1) // one success: still inside the hysteresis window
	if f.healthy(t, "worker") {
		t.Fatal("one success must not restore Healthy")
	}
	if !headless() {
		t.Error("worker inside the hysteresis window must stay headless")
	}
	f.poll(t, "worker", 1) // second success restores Healthy
	if !f.healthy(t, "worker") || headless() {
		t.Fatal("worker should be healthy and no longer headless after the success threshold")
	}
	f.down.Store(true)
	f.poll(t, "worker", 1) // Healthy=true, Failures=1
	n := nodeByName(t, f.r, "worker")
	n.mu.RLock()
	healthy, failures := n.Healthy, n.Failures
	n.mu.RUnlock()
	if !healthy || failures != 1 {
		t.Fatalf("state = healthy %v failures %d, want true 1", healthy, failures)
	}
	if !headless() {
		t.Error("a healthy worker with one failed probe is presented as headless")
	}
	if got := f.hook.settleWithSentinel(t, f.r); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	if v, ok := prevHealthyOf(f.r, "worker"); !ok || !v {
		t.Errorf("prevHealthy = %v,%v, want true,true untouched", v, ok)
	}
}

// A worker that is also draining is muted the same way: draining does not feed
// the node_down decision.
func TestMarkFailure_DrainingWorkerSendsNothing(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	w.mu.Lock()
	w.Draining = true
	w.mu.Unlock()
	failN(r, w, 3)
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
		t.Fatalf("events = %v, want none for a draining worker", got)
	}
}

// If role resolution panics, the worker is treated as unresolved and the
// node_down still fires, through markFailure directly and through the
// pollNode panic-recover path.
func TestMarkFailure_ResolverPanicFallsBackToNodeDown(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	r.resolveRoles = func([]*NodeState) (map[string]SchedulingRole, map[string]string) {
		panic("simulated resolver panic")
	}
	failN(r, nodeByName(t, r, "worker"), 3)
	if got := hook.waitFor(1); len(got) != 1 || got[0] != "node_down:worker" {
		t.Fatalf("events = %v, want node_down:worker", got)
	}
	if v, _ := prevHealthyOf(r, "worker"); v {
		t.Error("prevHealthy must record the node_down that was sent")
	}

	solo := nodeByName(t, r, "solo")
	solo.mu.Lock()
	solo.probe = panicProbe{}
	solo.mu.Unlock()
	for i := 0; i < 3; i++ {
		r.pollNode(solo)
	}
	got := hook.waitFor(2)
	if countEvent(got, "node_down:solo") != 1 {
		t.Fatalf("events = %v, want node_down:solo through the panic-recover path", got)
	}
}
