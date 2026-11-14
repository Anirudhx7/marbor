package router

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func blockedFixture(t *testing.T, runtime string, workers ...string) *Router {
	t.Helper()
	cfgs := []config.NodeConfig{{Name: "head", URL: "http://head.invalid:8000", Runtime: runtime, Host: "h0"}}
	for i, w := range workers {
		cfgs = append(cfgs, config.NodeConfig{Name: w, URL: fmt.Sprintf("http://%s.invalid:8000", w), Runtime: runtime, Host: fmt.Sprintf("h%d", i+1)})
	}
	cfgs = append(cfgs, config.NodeConfig{Name: "std", URL: "http://std.invalid:8000", Runtime: runtime, Host: "hs"})
	return replicaStaleRouter(t, cfgs, append([]string{"head"}, workers...)...)
}

func blockedHeadsFor(r *Router) map[string]string {
	roles, heads := r.SchedulingRolesWithHeads(r.nodes)
	return r.ReplicaBlockedHeads(r.nodes, roles, heads)
}

func TestReplicaBlockedHeads_MatchesHelperAcrossRuntimes(t *testing.T) {
	for _, rt := range []string{"ollama", "vllm", "llamacpp", "mlx"} {
		for _, tc := range []struct {
			name  string
			setup func(r *Router)
			want  string
		}{
			{"agent stale", func(r *Router) { setStaleByName(t, r, "worker", true) }, "worker"},
			{"agentless worker", func(r *Router) {}, ""},
			{"over-cap telemetry unknown", func(r *Router) {
				n := nodeByName(t, r, "worker")
				n.mu.Lock()
				n.AgentTelemetryUnknown = true
				n.mu.Unlock()
			}, ""},
			{"head own agent stale", func(r *Router) { setStaleByName(t, r, "head", true) }, ""},
		} {
			t.Run(rt+"/"+tc.name, func(t *testing.T) {
				r := blockedFixture(t, rt, "worker")
				tc.setup(r)
				got := blockedHeadsFor(r)
				roles, heads := resolveSchedulingRolesAndHeads(r.nodes)
				helper := unreachableReplicaHeads(r.nodes, roles, heads)
				if len(got) != len(helper) {
					t.Fatalf("wrapper %v differs from helper %v", got, helper)
				}
				if tc.want == "" {
					if len(got) != 0 {
						t.Fatalf("got %v, want none", got)
					}
					return
				}
				if got["head"] != tc.want || helper["head"] == nil || helper["head"].Name != tc.want {
					t.Fatalf("got %v, want head -> %s", got, tc.want)
				}
			})
		}
	}
}

func TestReplicaBlockedHeads_NilWhenNoHeads(t *testing.T) {
	r := blockedFixture(t, "vllm", "worker")
	roles, _ := r.SchedulingRolesWithHeads(r.nodes)
	if got := r.ReplicaBlockedHeads(r.nodes, roles, nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestReplicaBlockedHeads_HelperFailsOpenOverSubset(t *testing.T) {
	r := blockedFixture(t, "vllm", "worker")
	setStaleByName(t, r, "worker", true)
	roles, heads := r.SchedulingRolesWithHeads(r.nodes)
	var subset []*NodeState
	for _, n := range r.nodes {
		if n.Name != "worker" {
			subset = append(subset, n)
		}
	}
	if got := r.ReplicaBlockedHeads(subset, roles, heads); len(got) != 0 {
		t.Fatalf("subset without the worker blocked %v, want none (fail open)", got)
	}
	if got := r.ReplicaBlockedHeads(r.nodes, roles, heads); got["head"] != "worker" {
		t.Fatalf("full fleet got %v, want head -> worker", got)
	}
}

func TestReplicaBlockedHeads_ConflictingDeclarationsBlockNothing(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "h1", URL: "http://h1.invalid:8000", Host: "a"},
		{Name: "h2", URL: "http://h2.invalid:8000", Host: "b"},
		{Name: "w", URL: "http://w.invalid:8000", Host: "c"},
	}
	r := New(config.RoutingConfig{}, cfgs, nil)
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		switch n.Name {
		case "h1":
			n.ReplicaPeers = peers("h1", "h1", "w")
		case "h2":
			n.ReplicaPeers = peers("h2", "h2", "w")
		case "w":
			n.ReplicaPeers = peers("h1", "h1", "w")
		}
		n.mu.Unlock()
	}
	setStaleByName(t, r, "w", true)
	roles, heads := r.SchedulingRolesWithHeads(r.nodes)
	got := r.ReplicaBlockedHeads(r.nodes, roles, heads)
	for h, m := range got {
		if roles[h] != RoleHead || heads[m] != h {
			t.Fatalf("blocked head %q (member %q) is not a resolved head of that member", h, m)
		}
	}
	if len(got) > 1 {
		t.Fatalf("a worker blocked more than one head: %v", got)
	}
}

func TestUnreachableReplicaHeads_LowestNamedStaleMemberWins(t *testing.T) {
	r := blockedFixture(t, "vllm", "wb", "wa", "wc")
	setStaleByName(t, r, "wc", true)
	setStaleByName(t, r, "wb", true)
	setStaleByName(t, r, "wa", true)
	roles, heads := resolveSchedulingRolesAndHeads(r.nodes)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		nodes := append([]*NodeState(nil), r.nodes...)
		rng.Shuffle(len(nodes), func(a, b int) { nodes[a], nodes[b] = nodes[b], nodes[a] })
		down := unreachableReplicaHeads(nodes, roles, heads)
		if down["head"] == nil || down["head"].Name != "wa" {
			t.Fatalf("iteration %d: culprit = %v, want wa", i, down["head"])
		}
		if got := r.ReplicaBlockedHeads(nodes, roles, heads)["head"]; got != "wa" {
			t.Fatalf("iteration %d: wrapper culprit = %q, want wa", i, got)
		}
	}
}

func TestReplicaBlockedHeads_DownOrDrainingHeadStillReported(t *testing.T) {
	r := blockedFixture(t, "vllm", "worker")
	setStaleByName(t, r, "worker", true)
	h := nodeByName(t, r, "head")
	h.mu.Lock()
	h.Healthy = false
	h.Draining = true
	h.mu.Unlock()
	if got := blockedHeadsFor(r); got["head"] != "worker" {
		t.Fatalf("got %v, want head -> worker regardless of head health", got)
	}
}

func TestReplicaBlockedHeads_ClearsWhenAgentReturns(t *testing.T) {
	r := blockedFixture(t, "vllm", "worker")
	setStaleByName(t, r, "worker", true)
	if len(blockedHeadsFor(r)) != 1 {
		t.Fatal("expected a blocked head")
	}
	setStaleByName(t, r, "worker", false)
	if got := blockedHeadsFor(r); len(got) != 0 {
		t.Fatalf("got %v after agent recovered", got)
	}
}

func TestReplicaBlockedHeads_RaceWithAgentStaleWrites(t *testing.T) {
	r := blockedFixture(t, "vllm", "worker")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			setStaleByName(t, r, "worker", i%2 == 0)
		}
	}()
	for i := 0; i < 500; i++ {
		_ = blockedHeadsFor(r)
	}
	close(stop)
	wg.Wait()
}
