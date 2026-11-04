package router

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitRouter(t *testing.T, nodes []nrNode, timeoutMs, slaMs int) *Router {
	t.Helper()
	r := nrRouter(t, 1, nodes)
	r.SetQueueMaxDepth(10)
	r.SetQueueTimeout(timeoutMs)
	r.SetOverflowSLA(slaMs)
	return r
}

func doWait(r *Router, cloud bool) (WaitResult, time.Duration) {
	start := time.Now()
	depth0 := r.QueueDepth()
	res := r.WaitForNodeWithPrefixOpts(context.Background(), "model-x", "", "", "", WaitOpts{CloudUsable: func() bool { return cloud }})
	d := time.Since(start)
	// Every outcome must release the queue slot it claimed.
	if n := r.QueueDepth(); n != depth0 {
		panic(fmt.Sprintf("queue depth = %d after outcome %q, want %d", n, res.Outcome, depth0))
	}
	return res, d
}

// Hard classes return at once with cloud usable, claim no queue slot, and
// carry the class.
func TestWait_SkipHard_Immediate(t *testing.T) {
	cases := map[string][]nrNode{
		NoRouteAllNodesUnavailable: {{state: "unhealthy"}, {state: "draining"}},
		NoRouteNoNodeServesModel:   {{runtime: "vllm"}},
	}
	for want, nodes := range cases {
		t.Run(want, func(t *testing.T) {
			r := waitRouter(t, nodes, 5000, 2000)
			res, d := doWait(r, true)
			if res.Outcome != WaitSkipHard || res.NoRoute != want {
				t.Fatalf("outcome=%q noRoute=%q, want skip_hard/%s", res.Outcome, res.NoRoute, want)
			}
			if d > 50*time.Millisecond || res.Waited != 0 {
				t.Errorf("took %v, waited %v, want immediate", d, res.Waited)
			}
			if r.QueueDepth() != 0 {
				t.Errorf("queue depth = %d, want 0", r.QueueDepth())
			}
			if res.Decision == nil || res.Decision.NoRoute != want {
				t.Errorf("decision = %+v, want the no_candidate decision", res.Decision)
			}
		})
	}
}

// A healthy node at capacity waits for the SLA, not the queue timeout.
func TestWait_Saturated_SLA(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 5000, 300)
	res, d := doWait(r, true)
	if res.Outcome != WaitSLAExpired || res.NoRoute != NoRouteAtCapacityHealthy {
		t.Fatalf("outcome=%q noRoute=%q, want sla_expired/at_capacity_healthy", res.Outcome, res.NoRoute)
	}
	if d < 280*time.Millisecond || d > 800*time.Millisecond {
		t.Errorf("took %v, want about 300ms", d)
	}
	if res.Waited < 280*time.Millisecond || res.Waited > d {
		t.Errorf("Waited = %v, want about 300ms and not above the total %v", res.Waited, d)
	}
}

// A slot freed mid-wait is taken at once.
func TestWait_Saturated_SlotFrees(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 5000, 2000)
	n := r.nodes[0]
	go func() {
		time.Sleep(150 * time.Millisecond)
		r.DecrConn(n)
	}()
	res, d := doWait(r, true)
	if res.Outcome != WaitRouted || res.Node == nil {
		t.Fatalf("outcome=%q node=%v, want routed", res.Outcome, res.Node)
	}
	if d < 120*time.Millisecond || d > 700*time.Millisecond {
		t.Errorf("took %v, want about 150ms", d)
	}
}

// A node that goes down while a request is queued ends the wait early.
func TestWait_TurnsHardWhileQueued(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 5000, 4000)
	n := r.nodes[0]
	go func() {
		time.Sleep(200 * time.Millisecond)
		n.mu.Lock()
		n.Healthy = false
		n.mu.Unlock()
		atomic.StoreInt32(&n.ActiveConns, 0)
		r.DecrConn(n) // wake the waiter
	}()
	res, d := doWait(r, true)
	if res.Outcome != WaitSkipHard || res.NoRoute != NoRouteAllNodesUnavailable {
		t.Fatalf("outcome=%q noRoute=%q, want skip_hard/all_nodes_unavailable", res.Outcome, res.NoRoute)
	}
	if d < 150*time.Millisecond || d > 1200*time.Millisecond {
		t.Errorf("took %v, want about 200ms, far below the SLA", d)
	}
}

// Without cloud the wait ignores both the fast path and the SLA: every class
// waits the queue timeout, as the plain WaitForNode always did.
func TestWait_NoCloud_Unchanged(t *testing.T) {
	for name, nodes := range map[string][]nrNode{
		"hard":      {{state: "unhealthy"}},
		"saturated": {{state: "full"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := waitRouter(t, nodes, 400, 50)
			res, d := doWait(r, false)
			if res.Outcome != WaitQueueTimeout {
				t.Fatalf("outcome = %q, want queue_timeout", res.Outcome)
			}
			if d < 380*time.Millisecond || d > 900*time.Millisecond {
				t.Errorf("took %v, want the 400ms queue timeout", d)
			}
		})
	}
}

// An SLA of 0 means the full queue timeout, and the SLA never exceeds it.
func TestWait_SLAZeroAndOverTimeout(t *testing.T) {
	for _, sla := range []int{0, 5000} {
		r := waitRouter(t, []nrNode{{state: "full"}}, 300, sla)
		res, d := doWait(r, true)
		if res.Outcome != WaitQueueTimeout {
			t.Fatalf("sla=%d: outcome = %q, want queue_timeout", sla, res.Outcome)
		}
		if d < 280*time.Millisecond || d > 800*time.Millisecond {
			t.Errorf("sla=%d: took %v, want about 300ms", sla, d)
		}
	}
}

func TestWait_QueueDisabledAndFull(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 0, 0)
	if res, _ := doWait(r, true); res.Outcome != WaitQueueDisabled {
		t.Errorf("outcome = %q, want queue_disabled", res.Outcome)
	}
	r = waitRouter(t, []nrNode{{state: "full"}}, 5000, 0)
	atomic.StoreInt32(&r.queueDepth, 10)
	if res, _ := doWait(r, true); res.Outcome != WaitQueueFull {
		t.Errorf("outcome = %q, want queue_full", res.Outcome)
	}
}

func TestWait_ContextCancelled(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 5000, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	res := r.WaitForNodeWithPrefixOpts(ctx, "model-x", "", "", "", WaitOpts{CloudUsable: func() bool { return true }})
	if res.Outcome != WaitCtxCancelled {
		t.Errorf("outcome = %q, want ctx_cancelled", res.Outcome)
	}
}

// A never-polled non-Ollama node is waitable (unknown state), bounded by the SLA.
func TestWait_UnknownStateWaits(t *testing.T) {
	r := waitRouter(t, []nrNode{{runtime: "vllm", state: "unpolled"}}, 5000, 300)
	res, d := doWait(r, true)
	if res.Outcome != WaitSLAExpired || res.NoRoute != NoRouteUnknownState {
		t.Fatalf("outcome=%q noRoute=%q, want sla_expired/unknown_state", res.Outcome, res.NoRoute)
	}
	if d < 280*time.Millisecond || d > 800*time.Millisecond {
		t.Errorf("took %v, want about 300ms", d)
	}
}

// The setters run concurrently with many waiters under the race detector.
// It has no assertions on purpose: the race detector is the check.
func TestWait_SettersLiveRace(t *testing.T) {
	r := waitRouter(t, []nrNode{{state: "full"}}, 200, 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.WaitForNodeWithPrefixOpts(context.Background(), "model-x", "", "", "", WaitOpts{CloudUsable: func() bool { return true }})
		}()
	}
	for i := 0; i < 50; i++ {
		r.SetQueueTimeout(150 + i)
		r.SetOverflowSLA(50 + i)
		r.SetQueueMaxDepth(5 + i)
	}
	wg.Wait()
}
