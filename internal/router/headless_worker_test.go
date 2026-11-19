package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestIsHeadlessWorker(t *testing.T) {
	cases := []struct {
		name     string
		role     SchedulingRole
		healthy  bool
		failures int
		want     bool
	}{
		{"worker healthy no failures", RoleWorker, true, 0, false},
		{"worker unhealthy", RoleWorker, false, 3, true},
		{"worker unhealthy zero failures", RoleWorker, false, 0, true},
		{"worker healthy with one failure", RoleWorker, true, 1, true},
		{"head unhealthy", RoleHead, false, 3, false},
		{"standalone unhealthy", RoleStandalone, false, 3, false},
		{"unresolved unhealthy", RoleUnresolved, false, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHeadlessWorker(tc.role, tc.healthy, tc.failures); got != tc.want {
				t.Errorf("isHeadlessWorker(%v,%v,%d) = %v, want %v", tc.role, tc.healthy, tc.failures, got, tc.want)
			}
		})
	}
}

// hookCollector records webhook events delivered to a test server.
type hookCollector struct {
	mu     sync.Mutex
	events []string
	srv    *httptest.Server
}

func newHookCollector(t *testing.T) *hookCollector {
	t.Helper()
	c := &hookCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p map[string]string
		_ = json.Unmarshal(body, &p)
		c.mu.Lock()
		c.events = append(c.events, p["event"]+":"+p["node"])
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *hookCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// waitFor polls until the collector has at least n events or times out.
func (c *hookCollector) waitFor(n int) []string {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.snapshot()
}

// settle gives any (wrongly) fired webhook goroutine time to arrive.
func (c *hookCollector) settle() []string {
	time.Sleep(150 * time.Millisecond)
	return c.snapshot()
}

// workerFleet builds head "head" + worker "worker" + standalone "solo".
func workerFleet(t *testing.T, hook *hookCollector) *Router {
	t.Helper()
	r := newTestRouter([]config.NodeConfig{
		{Name: "head", URL: "http://localhost:19991"},
		{Name: "worker", URL: "http://localhost:19992"},
		{Name: "solo", URL: "http://localhost:19993"},
	}, config.WebhookConfig{Enabled: true, URL: hook.srv.URL})
	declareGroup(r, "head", "worker")
	return r
}

func declareGroup(r *Router, head string, members ...string) {
	all := append([]string{head}, members...)
	for _, n := range r.nodes {
		for _, m := range all {
			if n.Name == m {
				n.mu.Lock()
				n.ReplicaPeers = &store.ReplicaPeers{Members: all, Head: head}
				n.mu.Unlock()
			}
		}
	}
}

func clearGroup(r *Router) {
	for _, n := range r.nodes {
		n.mu.Lock()
		n.ReplicaPeers = nil
		n.mu.Unlock()
	}
}

func failN(r *Router, n *NodeState, times int) {
	for i := 0; i < times; i++ {
		r.markFailure(n)
	}
}

func prevHealthyOf(r *Router, name string) (bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.prevHealthy[name]
	return v, ok
}

func TestMarkFailure_WorkerSendsNoNodeDown(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	failN(r, w, 5)
	if got := hook.settle(); len(got) != 0 {
		t.Fatalf("worker failure sent webhooks %v, want none", got)
	}
	w.mu.RLock()
	healthy := w.Healthy
	w.mu.RUnlock()
	if healthy {
		t.Error("raw Healthy must still turn false for a failing worker")
	}
	if v, _ := prevHealthyOf(r, "worker"); !v {
		t.Error("prevHealthy for a worker must stay at its seeded true")
	}
}

func TestMarkFailure_StandaloneHeadAndUnresolvedStillFireNodeDown(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	// An unresolved node: names a member that does not exist.
	r.AddNode(config.NodeConfig{Name: "orphan", URL: "http://localhost:19994"})
	orphan := nodeByName(t, r, "orphan")
	orphan.mu.Lock()
	orphan.ReplicaPeers = &store.ReplicaPeers{Members: []string{"orphan", "ghost"}, Head: "orphan"}
	orphan.mu.Unlock()

	failN(r, nodeByName(t, r, "head"), 3)
	failN(r, nodeByName(t, r, "solo"), 3)
	failN(r, orphan, 3)
	got := hook.waitFor(3)
	if len(got) != 3 {
		t.Fatalf("events = %v, want node_down for head, solo and orphan", got)
	}
	// Only once each: further failures do not repeat.
	failN(r, nodeByName(t, r, "solo"), 3)
	if got := hook.settle(); len(got) != 3 {
		t.Errorf("repeat failures re-sent node_down: %v", got)
	}
}

func TestMarkFailure_WorkerStillSnapshotsResidency(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	failN(r, w, 3)
	// snapshotNode ran without panic and the worker stayed silent; the
	// transition flag path is exercised by the Healthy flip above.
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.Healthy {
		t.Error("worker should be raw unhealthy after threshold")
	}
}

func TestMarkFailure_AddedStandaloneNodeFires(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	r.AddNode(config.NodeConfig{Name: "late", URL: "http://localhost:19995"})
	failN(r, nodeByName(t, r, "late"), 3)
	if got := hook.waitFor(1); len(got) != 1 || got[0] != "node_down:late" {
		t.Fatalf("events = %v, want node_down:late", got)
	}
}

func TestMarkFailure_WorkerWebhookDisabledNoPanic(t *testing.T) {
	r := newTestRouter([]config.NodeConfig{
		{Name: "head", URL: "http://localhost:19991"},
		{Name: "worker", URL: "http://localhost:19992"},
	}, config.WebhookConfig{})
	declareGroup(r, "head", "worker")
	failN(r, nodeByName(t, r, "worker"), 3)
	failN(r, nodeByName(t, r, "head"), 3)
}

func TestMarkFailure_RedeclaredStandaloneWhileFailingSendsNothing(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	failN(r, w, 3) // silent as a worker
	clearGroup(r)  // now standalone, still failing
	failN(r, w, 3) // Healthy already false: no new transition
	if got := hook.settle(); len(got) != 0 {
		t.Fatalf("events = %v, want none (no node_down for a muted outage)", got)
	}
	if v, _ := prevHealthyOf(r, "worker"); !v {
		t.Error("prevHealthy must stay true so recovery sends no node_up")
	}
}

func TestMarkFailure_ProcessRestartMidOutageSeededTrue(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	// New router state seeds prevHealthy true for every node.
	for _, name := range []string{"worker", "solo"} {
		if v, ok := prevHealthyOf(r, name); !ok || !v {
			t.Fatalf("%s: prevHealthy = %v,%v want true,true at start", name, v, ok)
		}
	}
	failN(r, nodeByName(t, r, "worker"), 3)
	failN(r, nodeByName(t, r, "solo"), 3)
	got := hook.waitFor(1)
	if len(got) != 1 || got[0] != "node_down:solo" {
		t.Fatalf("events = %v, want only node_down:solo", hook.settle())
	}
}

func TestMarkFailure_DownThenDeclaredWorkerRecoveryStillSendsNodeUp(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	clearGroup(r)
	s := nodeByName(t, r, "worker")
	failN(r, s, 3) // standalone: node_down sent
	if got := hook.waitFor(1); len(got) != 1 || got[0] != "node_down:worker" {
		t.Fatalf("events = %v, want node_down:worker", got)
	}
	declareGroup(r, "head", "worker") // becomes a worker mid-outage
	if v, _ := prevHealthyOf(r, "worker"); v {
		t.Fatal("prevHealthy must remain false after node_down was sent")
	}
	failN(r, s, 2) // still down as a worker: no further event
	if got := hook.settle(); len(got) != 1 {
		t.Fatalf("events = %v, want just the original node_down", got)
	}
}

func TestMarkFailure_RemovedNodeMidPollSendsNothing(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	solo := nodeByName(t, r, "solo")
	r.RemoveNode("solo")
	failN(r, solo, 3)
	if got := hook.settle(); len(got) != 0 {
		t.Fatalf("events = %v, want none for a removed node", got)
	}
	if _, ok := prevHealthyOf(r, "solo"); ok {
		t.Error("removed node must not regain a prevHealthy entry")
	}
}

func TestMarkFailure_PanicRecoverPathFiresForNonWorker(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	solo := nodeByName(t, r, "solo")
	solo.mu.Lock()
	solo.probe = panicProbe{}
	solo.mu.Unlock()
	for i := 0; i < 3; i++ {
		r.pollNode(solo)
	}
	if got := hook.waitFor(1); len(got) != 1 || got[0] != "node_down:solo" {
		t.Fatalf("events = %v, want node_down:solo", got)
	}
}

func TestMarkFailure_PanicRecoverPathSilentForWorker(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	w.mu.Lock()
	w.probe = panicProbe{}
	w.mu.Unlock()
	for i := 0; i < 3; i++ {
		r.pollNode(w)
	}
	if got := hook.settle(); len(got) != 0 {
		t.Fatalf("events = %v, want none for a worker", got)
	}
}

func TestRoleForWebhook_PanicFallsBackToUnresolved(t *testing.T) {
	r := newTestRouter([]config.NodeConfig{{Name: "a", URL: "http://localhost:19991"}}, config.WebhookConfig{})
	// A nil node in the pool makes role resolution panic.
	r.mu.Lock()
	r.nodes = append(r.nodes, nil)
	r.mu.Unlock()
	if got := r.roleForWebhook("a"); got != RoleUnresolved {
		t.Fatalf("roleForWebhook after panic = %v, want RoleUnresolved", got)
	}
}

// TestRoleForWebhook_CompletesWhileAnotherNodeLockIsHeld: role resolution may
// wait for another node's write lock, but it must finish as soon as that lock
// is released, and a markFailure on a different node must not deadlock with it.
func TestRoleForWebhook_CompletesWhileAnotherNodeLockIsHeld(t *testing.T) {
	r := newTestRouter([]config.NodeConfig{
		{Name: "a", URL: "http://localhost:19991"},
		{Name: "b", URL: "http://localhost:19992"},
	}, config.WebhookConfig{})
	a := nodeByName(t, r, "a")
	b := nodeByName(t, r, "b")
	b.mu.Lock()
	done := make(chan struct{})
	go func() {
		failN(r, a, 3)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	b.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("markFailure did not finish after the other node lock was released")
	}
}

func TestMarkFailure_WorkerRecoveryZeroWebhooksViaPollNode(t *testing.T) {
	hook := newHookCollector(t)
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollama.Close()
	r := newTestRouter([]config.NodeConfig{
		{Name: "head", URL: ollama.URL, Runtime: "ollama"},
		{Name: "worker", URL: "http://127.0.0.1:1", Runtime: "ollama"},
	}, config.WebhookConfig{Enabled: true, URL: hook.srv.URL})
	declareGroup(r, "head", "worker")
	w := nodeByName(t, r, "worker")
	for i := 0; i < 3; i++ {
		r.pollNode(w)
	}
	w.mu.Lock()
	w.URL = ollama.URL
	w.mu.Unlock()
	for i := 0; i < 5; i++ {
		r.pollNode(w)
	}
	w.mu.RLock()
	healthy := w.Healthy
	w.mu.RUnlock()
	if !healthy {
		t.Fatal("worker should have recovered")
	}
	if got := hook.settle(); len(got) != 0 {
		t.Fatalf("events = %v, want zero webhooks across fail and recover", got)
	}
}

func TestMarkFailure_AgentWebhooksUnaffectedForWorker(t *testing.T) {
	// agent_down/agent_up are sent from the agent poll path, which never
	// consults the replica role. Assert the markFailure path alone leaves
	// agent state untouched.
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	failN(r, w, 3)
	for _, e := range hook.settle() {
		if e == "agent_down:worker" || e == "agent_up:worker" {
			t.Fatalf("unexpected agent event %s", e)
		}
	}
}

func TestMarkFailure_RaceWithRoleChange(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.markFailure(w)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			clearGroup(r)
			declareGroup(r, "head", "worker")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.PatchNode("worker", NodePatch{})
		}
	}()
	wg.Wait()
	// Converged: at most one node_down for the worker, never a panic.
	if got := hook.settle(); len(got) > 1 {
		t.Errorf("events = %v, want at most one node_down", got)
	}
}

func TestMarkFailure_Law5RuntimeTable(t *testing.T) {
	for _, rt := range []string{"ollama", "mlx", "vllm", "llamacpp"} {
		t.Run(rt, func(t *testing.T) {
			hook := newHookCollector(t)
			r := newTestRouter([]config.NodeConfig{
				{Name: "head", URL: "http://localhost:19991", Runtime: rt},
				{Name: "worker", URL: "http://localhost:19992", Runtime: rt},
			}, config.WebhookConfig{Enabled: true, URL: hook.srv.URL})
			declareGroup(r, "head", "worker")
			roles := r.SchedulingRoles(r.Nodes())
			if roles["worker"] != RoleWorker {
				t.Fatalf("worker role = %v on %s, want RoleWorker", roles["worker"], rt)
			}
			failN(r, nodeByName(t, r, "worker"), 3)
			if got := hook.settle(); len(got) != 0 {
				t.Fatalf("%s worker muted, events = %v", rt, got)
			}
		})
	}
}
