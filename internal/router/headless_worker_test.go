package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
			if got := IsHeadlessWorker(tc.role, tc.healthy, tc.failures); got != tc.want {
				t.Errorf("IsHeadlessWorker(%v,%v,%d) = %v, want %v", tc.role, tc.healthy, tc.failures, got, tc.want)
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

// settle gives any (wrongly) fired webhook goroutine time to arrive. It is a
// bounded wait for a NEGATIVE assertion (an event that must not appear), so it
// can only ever miss a very late wrong event, never fail a correct run. Where
// the assertion matters most, use settleWithSentinel instead.
func (c *hookCollector) settle() []string {
	time.Sleep(150 * time.Millisecond)
	return c.snapshot()
}

// settleWithSentinel waits for a real node_down from a throwaway standalone
// node, which was started after the code under test, then returns every event
// except the sentinel's. A wrongly fired webhook from earlier work would have
// been queued ahead of the sentinel's, so it is far less likely to be missed
// than with a plain sleep. Delivery order is not guaranteed, so a short
// grace period still follows.
var sentinelSeq atomic.Int64

// waitForEvent polls until the event has arrived or times out.
func (c *hookCollector) waitForEvent(want string) []string {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); countEvent(got, want) > 0 {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.snapshot()
}

func countEvent(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

func (c *hookCollector) settleWithSentinel(t *testing.T, r *Router) []string {
	t.Helper()
	seq := sentinelSeq.Add(1)
	name := fmt.Sprintf("sentinel-%d", seq)
	// A distinct URL per sentinel: the router refuses one backend under two names.
	r.AddNode(config.NodeConfig{Name: name, URL: fmt.Sprintf("http://localhost:%d", 20000+seq%40000)})
	failN(r, nodeByName(t, r, name), 3)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range c.snapshot() {
			if e == "node_down:"+name {
				time.Sleep(50 * time.Millisecond)
				var out []string
				for _, e := range c.snapshot() {
					if !strings.HasPrefix(e, "node_down:sentinel-") {
						out = append(out, e)
					}
				}
				return out
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sentinel node_down never arrived")
	return nil
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
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
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
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if want := []string{"node_down:head", "node_down:orphan", "node_down:solo"}; !reflect.DeepEqual(sorted, want) {
		t.Fatalf("events = %v, want %v", sorted, want)
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
	st := openWarmTestStore(t)
	r.SetStore(st)
	w := nodeByName(t, r, "worker")
	w.mu.Lock()
	w.LoadedModels = []ModelInfo{{Name: "big", SizeVRAM: 4096}}
	w.mu.Unlock()
	failN(r, w, 3)
	// The warm set is captured on the unhealthy transition for every role, a
	// muted worker included, so its residency survives the outage.
	rows, err := st.AllWarmState()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Node != "worker" || rows[0].Model != "big" || rows[0].VRAMBytes != 4096 {
		t.Fatalf("warm state after worker went unhealthy = %+v, want one row for big on worker", rows)
	}
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
	if v, ok := prevHealthyOf(r, "worker"); !ok || !v {
		t.Errorf("worker prevHealthy = %v,%v, want true,true", v, ok)
	}
	if v, ok := prevHealthyOf(r, "head"); !ok || v {
		t.Errorf("head prevHealthy = %v,%v, want false,true (node_down recorded even with webhooks off)", v, ok)
	}
	for _, name := range []string{"worker", "head"} {
		n := nodeByName(t, r, name)
		n.mu.RLock()
		healthy := n.Healthy
		n.mu.RUnlock()
		if healthy {
			t.Errorf("%s should be raw unhealthy", name)
		}
	}
}

func TestMarkFailure_RedeclaredStandaloneWhileFailingSendsNothing(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	failN(r, w, 3) // silent as a worker
	clearGroup(r)  // now standalone, still failing
	failN(r, w, 3) // Healthy already false: no new transition
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
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

func TestMarkFailure_RemovedNodeMidPollSendsNothing(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	solo := nodeByName(t, r, "solo")
	r.RemoveNode("solo")
	failN(r, solo, 3)
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
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
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
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

// TestRoleForWebhook_HoldsNoLockWhileResolving: role resolution reads every
// node under that node's own lock, so it must run with no lock of this node or
// the router held. While resolution is blocked on node b's write lock, this
// test must still be able to take node a's lock and the router lock; hoisting
// the resolution under either would leave those acquisitions blocked.
func TestRoleForWebhook_HoldsNoLockWhileResolving(t *testing.T) {
	r := newTestRouter([]config.NodeConfig{
		{Name: "a", URL: "http://localhost:19991"},
		{Name: "b", URL: "http://localhost:19992"},
	}, config.WebhookConfig{})
	a := nodeByName(t, r, "a")
	b := nodeByName(t, r, "b")
	started := make(chan struct{})
	r.resolveRoles = func(nodes []*NodeState) (map[string]SchedulingRole, map[string]string) {
		close(started)
		return resolveSchedulingRolesAndHeads(nodes)
	}
	b.mu.Lock() // another node is being written
	done := make(chan struct{})
	go func() {
		failN(r, a, 3)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		b.mu.Unlock()
		t.Fatal("role resolution never started")
	}
	acquired := make(chan struct{})
	go func() {
		a.mu.Lock()
		a.mu.Unlock()
		r.mu.Lock()
		r.mu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		b.mu.Unlock()
		t.Fatal("node a lock or router lock is held while roles resolve (lock hoisted)")
	}
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
	w.mu.RLock()
	wentDown := !w.Healthy
	w.mu.RUnlock()
	if !wentDown {
		t.Fatal("worker should have gone unhealthy before it recovered")
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
	if got := hook.settleWithSentinel(t, r); len(got) != 0 {
		t.Fatalf("events = %v, want zero webhooks across fail and recover", got)
	}
	if v, ok := prevHealthyOf(r, "worker"); !ok || !v {
		t.Errorf("worker prevHealthy = %v,%v after recovery, want true,true", v, ok)
	}
}

// agent_down and agent_up come from the agent poll path, which never
// consults the replica role: a worker's agent still announces both.
func TestAgentWebhooks_UnaffectedForWorker(t *testing.T) {
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	r.mu.Lock()
	r.prevAgentPresent["worker"] = true
	r.mu.Unlock()
	for i := 0; i < 3; i++ {
		r.agentUnreachable(w)
	}
	if got := hook.waitFor(1); len(got) != 1 || got[0] != "agent_down:worker" {
		t.Fatalf("events = %v, want agent_down:worker", got)
	}
	r.agentReachable("worker")
	got := hook.waitFor(2)
	if len(got) != 2 {
		t.Fatalf("events = %v, want agent_down then agent_up for the worker", got)
	}
	sort.Strings(got)
	if got[0] != "agent_down:worker" || got[1] != "agent_up:worker" {
		t.Fatalf("events = %v, want agent_down:worker and agent_up:worker", got)
	}
}

func setPeers(t *testing.T, r *Router, head string, members []string, clear bool) {
	t.Helper()
	for _, name := range members {
		rp := &store.ReplicaPeers{Members: members, Head: head}
		if clear {
			rp = &store.ReplicaPeers{}
		}
		if !r.PatchNode(name, NodePatch{ReplicaPeers: rp}) {
			t.Errorf("PatchNode %s failed", name) // may run off the test goroutine
		}
	}
}

func resetNode(n *NodeState) {
	n.mu.Lock()
	n.Healthy = true
	n.Failures = 0
	n.ConsecutiveSuccesses = 0
	n.mu.Unlock()
}

// TestMarkFailure_RaceWithRoleChange flips the worker's real role through
// PatchNode (declare the group, then clear it) while markFailure runs, with the
// node reset each round so every round can cross the failure threshold again.
// Whatever the interleaving, a node_down for the worker is recorded exactly
// when one was sent, and once the role stops moving the converged behaviour
// holds: silent as a worker, announced as a standalone node.
func TestMarkFailure_RaceWithRoleChange(t *testing.T) {
	const rounds = 60
	hook := newHookCollector(t)
	r := workerFleet(t, hook)
	w := nodeByName(t, r, "worker")
	members := []string{"head", "worker"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			resetNode(w)
			failN(r, w, 3)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			setPeers(t, r, "head", members, i%2 == 1)
		}
	}()
	wg.Wait()

	got := hook.settleWithSentinel(t, r)
	if len(got) > rounds {
		t.Fatalf("%d events for %d rounds: %v", len(got), rounds, got)
	}
	for _, e := range got {
		if e != "node_down:worker" {
			t.Fatalf("unexpected event %q in %v", e, got)
		}
	}
	if v, ok := prevHealthyOf(r, "worker"); !ok || v != (len(got) == 0) {
		t.Fatalf("prevHealthy = %v,%v with %d node_down events: must be false exactly when one was sent", v, ok, len(got))
	}

	// Converged as a worker: silent.
	setPeers(t, r, "head", members, false)
	before := len(got)
	resetNode(w)
	failN(r, w, 3)
	if after := hook.settleWithSentinel(t, r); len(after) != before {
		t.Fatalf("worker role: %d events after, %d before; want no new node_down", len(after), before)
	}
	// Converged as standalone: announced.
	setPeers(t, r, "head", members, true)
	resetNode(w)
	failN(r, w, 3)
	deadline := time.Now().Add(2 * time.Second)
	for countEvent(hook.snapshot(), "node_down:worker") < before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := countEvent(hook.snapshot(), "node_down:worker"); n != before+1 {
		t.Fatalf("standalone role: %d node_down:worker events, want %d", n, before+1)
	}
}

// Many polls of one node at once: the unhealthy transition happens once, so a
// standalone node announces once and a worker never does.
func TestPollNode_ConcurrentPollsAnnounceOnce(t *testing.T) {
	f := newSwitchFleet(t)
	f.down.Store(true)
	var wg sync.WaitGroup
	for _, name := range []string{"solo", "worker"} {
		n := nodeByName(t, f.r, name)
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					f.r.pollNode(n)
				}
			}()
		}
	}
	wg.Wait()
	got := f.hook.settleWithSentinel(t, f.r)
	if len(got) != 1 || got[0] != "node_down:solo" {
		t.Fatalf("events = %v, want exactly node_down:solo", got)
	}
}

// The rows differ only in the runtime string: the worker role is a
// declaration, so muting must not depend on which runtime the node runs.
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
			if got := hook.settleWithSentinel(t, r); len(got) != 0 {
				t.Fatalf("%s worker muted, events = %v", rt, got)
			}
		})
	}
}
