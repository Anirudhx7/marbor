package router

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

type nrNode struct {
	runtime string
	state   string // "", "unhealthy", "draining", "full", "unpolled"
}

func nrRouter(t *testing.T, maxInFlight int, nodes []nrNode) *Router {
	t.Helper()
	var cfgs []config.NodeConfig
	for i, n := range nodes {
		cfgs = append(cfgs, config.NodeConfig{
			Name: fmt.Sprintf("n%02d", i), URL: fmt.Sprintf("http://n%02d:11434", i),
			Runtime: n.runtime, VRAMTotalMB: 8192,
		})
	}
	r := New(config.RoutingConfig{Strategy: "warm-first", MaxInFlightPerNode: maxInFlight}, cfgs, nil)
	for i, n := range nodes {
		ns := r.nodes[i]
		ns.mu.Lock()
		ns.LastPollAt = time.Now()
		switch n.state {
		case "unhealthy":
			ns.Healthy = false
		case "draining":
			ns.Draining = true
		case "unpolled":
			ns.LastPollAt = time.Time{}
		}
		ns.mu.Unlock()
		if n.state == "full" {
			atomic.StoreInt32(&ns.ActiveConns, int32(maxInFlight))
		}
	}
	return r
}

func TestClassify_NoRoute(t *testing.T) {
	many := func(n int, nn nrNode) []nrNode {
		out := make([]nrNode, n)
		for i := range out {
			out[i] = nn
		}
		return out
	}
	cases := []struct {
		name  string
		nodes []nrNode
		rt    string
		want  string
	}{
		{"zero nodes", nil, "", NoRouteNoNodeServesModel},
		{"all runtime mismatch", []nrNode{{runtime: "vllm"}, {runtime: "vllm"}}, "ollama", NoRouteNoNodeServesModel},
		{"all unhealthy", []nrNode{{state: "unhealthy"}, {state: "unhealthy"}}, "", NoRouteAllNodesUnavailable},
		{"all draining", []nrNode{{state: "draining"}}, "", NoRouteAllNodesUnavailable},
		{"draining plus full", []nrNode{{state: "draining"}, {state: "full"}}, "", NoRouteAtCapacityHealthy},
		{"unhealthy plus ineligible", []nrNode{{state: "unhealthy"}, {runtime: "vllm"}}, "", NoRouteAllNodesUnavailable},
		{"vllm without model, polled", []nrNode{{runtime: "vllm"}}, "", NoRouteNoNodeServesModel},
		{"vllm without model, never polled", []nrNode{{runtime: "vllm", state: "unpolled"}}, "", NoRouteUnknownState},
		{"one full among unhealthy", []nrNode{{state: "unhealthy"}, {state: "full"}, {state: "unhealthy"}}, "", NoRouteAtCapacityHealthy},
		{"25 unhealthy beyond the excluded cap", many(25, nrNode{state: "unhealthy"}), "", NoRouteAllNodesUnavailable},
		{"25 unhealthy then one full beyond the cap", append(many(25, nrNode{state: "unhealthy"}), nrNode{state: "full"}), "", NoRouteAtCapacityHealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := nrRouter(t, 1, c.nodes)
			node, _, d := r.Route("model-x", "", c.rt)
			if node != nil {
				t.Fatalf("expected no node, got %s", node.Name)
			}
			if d == nil || d.Reason != ReasonNoCandidate {
				t.Fatalf("decision = %+v, want no_candidate", d)
			}
			if d.NoRoute != c.want {
				t.Errorf("NoRoute = %q, want %q", d.NoRoute, c.want)
			}
		})
	}
}

// An unrecognised exclusion reason must classify as waitable, never as a hard
// class that would skip the queue.
func TestClassify_UnknownReasonIsWaitable(t *testing.T) {
	f := candidateFilter{nodesSeen: 1, nUnknown: 1}
	if got := f.classifyNoRoute(); got != NoRouteUnknownState || IsHardNoRoute(got) {
		t.Errorf("classifyNoRoute = %q, want waitable unknown_state", got)
	}
	f = candidateFilter{nodesSeen: 3}
	if got := f.classifyNoRoute(); IsHardNoRoute(got) {
		t.Errorf("empty counters classified hard: %q", got)
	}
}

// NoRoute is only set on a no_candidate decision; a routed request never
// carries it.
func TestClassify_NoRouteAbsentWhenRouted(t *testing.T) {
	r := nrRouter(t, 1, []nrNode{{}, {state: "unhealthy"}})
	node, _, d := r.Route("model-x", "", "")
	if node == nil || d == nil || d.NoRoute != "" {
		t.Fatalf("node=%v decision=%+v, want a node and empty NoRoute", node, d)
	}
}

// Placement is unchanged: two identically built routers, one asked through
// Route and one through RouteExcluding with no exclusions, choose the same
// node (or both none) for every request.
func TestClassify_PlacementUnchanged(t *testing.T) {
	var nodes []nrNode
	for _, s := range []string{"", "unhealthy", "draining", "full", ""} {
		nodes = append(nodes, nrNode{state: s})
	}
	a := nrRouter(t, 2, nodes)
	b := nrRouter(t, 2, nodes)
	for i := 0; i < 20; i++ {
		na, _, _ := a.Route("model-x", "", "")
		nb, _, _ := b.RouteExcluding("model-x", "", nil)
		if (na == nil) != (nb == nil) || (na != nil && na.Name != nb.Name) {
			t.Fatalf("iteration %d: Route=%v RouteExcluding=%v differ", i, na, nb)
		}
	}
}
