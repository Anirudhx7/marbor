package router

// max_in_flight_test.go -- locks in the per-node in-flight cap: a node at
// or over its effective cap (per-node override, or the global
// RoutingConfig.MaxInFlightPerNode default) is a hard-ineligible routing
// candidate (same eligibility tier as isEligibleForModel),
// shed immediately rather than queued, with failover handled entirely by the
// existing RouteExcluding/retry chain.

import (
	"sync/atomic"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestMaxInFlightCapExcludesNodeAtCapacity verifies a node at its global
// in-flight cap is never selected by Route while another eligible node is
// available, and that the under-cap node IS selected instead.
func TestMaxInFlightCapExcludesNodeAtCapacity(t *testing.T) {
	r := New(config.RoutingConfig{MaxInFlightPerNode: 2}, []config.NodeConfig{
		{Name: "busy-node", URL: "http://busy.invalid", Runtime: "ollama"},
		{Name: "free-node", URL: "http://free.invalid", Runtime: "ollama"},
	}, nil)

	var busyNode, freeNode *NodeState
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
		switch n.Name {
		case "busy-node":
			busyNode = n
		case "free-node":
			freeNode = n
		}
	}
	if busyNode == nil || freeNode == nil {
		t.Fatal("expected both nodes to be present")
	}
	atomic.StoreInt32(&busyNode.ActiveConns, 2) // at cap (>=2)

	for i := 0; i < 10; i++ {
		node, _, _ := r.Route("", "", "")
		if node == nil {
			t.Fatal("expected a node to be selected")
		}
		if node == busyNode {
			t.Fatalf("busy-node at cap must never be selected while free-node is under cap, got %q", node.Name)
		}
	}
}

// TestMaxInFlightCapZeroMeansUncapped verifies the default (0, unset) global
// cap preserves the historical uncapped behavior: a node may accumulate any number
// of in-flight requests and remain eligible.
func TestMaxInFlightCapZeroMeansUncapped(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "solo-node", URL: "http://solo.invalid", Runtime: "ollama"},
	}, nil)

	var n *NodeState
	for _, node := range r.Nodes() {
		node.Lock()
		node.Healthy = true
		node.Unlock()
		n = node
	}
	atomic.StoreInt32(&n.ActiveConns, 1000) // absurdly high, should not matter

	node, _, _ := r.Route("", "", "")
	if node != n {
		t.Fatalf("expected the only node to remain eligible when no cap is configured, got %v", node)
	}
}

// TestMaxInFlightPerNodeOverrideBeatsGlobalDefault verifies a per-node
// MaxInFlight override (NodeConfig.MaxInFlight, resolved into
// NodeState.MaxInFlight) takes precedence over the global default in both
// directions: a tighter per-node cap sheds the node earlier than the global
// default would, and a looser per-node cap keeps it eligible past the global
// default's threshold.
func TestMaxInFlightPerNodeOverrideBeatsGlobalDefault(t *testing.T) {
	r := New(config.RoutingConfig{MaxInFlightPerNode: 10}, []config.NodeConfig{
		{Name: "tight-node", URL: "http://tight.invalid", Runtime: "ollama", MaxInFlight: 2},
		{Name: "loose-node", URL: "http://loose.invalid", Runtime: "ollama"},
	}, nil)

	var tightNode *NodeState
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
		if n.Name == "tight-node" {
			tightNode = n
		}
	}
	// tight-node is at its own override (2) but well under the global default
	// (10) - the override must still apply.
	atomic.StoreInt32(&tightNode.ActiveConns, 2)

	for i := 0; i < 10; i++ {
		node, _, _ := r.Route("", "", "")
		if node == tightNode {
			t.Fatalf("tight-node's per-node override (2) must apply even though the global default (10) would still allow it")
		}
		if node == nil {
			t.Fatal("expected loose-node to remain eligible")
		}
	}
}

// TestMaxInFlightStickySessionDoesNotBypassCap verifies a session pinned to a
// node that is at/over its cap does not reuse that node just because it's
// pinned - the sticky shortcut must re-check capacity, same as it already
// re-checks model eligibility.
func TestMaxInFlightStickySessionDoesNotBypassCap(t *testing.T) {
	r := New(config.RoutingConfig{SessionAffinity: true, MaxInFlightPerNode: 1}, []config.NodeConfig{
		{Name: "aa-pinned-node", URL: "http://pinned.invalid", Runtime: "ollama"},
		{Name: "other-node", URL: "http://other.invalid", Runtime: "ollama"},
	}, nil)

	var pinnedNode *NodeState
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
		if n.Name == "aa-pinned-node" {
			pinnedNode = n
		}
	}
	if pinnedNode == nil {
		t.Fatal("pinned-node not found")
	}

	// Establish a sticky-session pin to pinned-node while it's still under cap.
	node, _, _ := r.Route("", "sess-1", "")
	if node != pinnedNode {
		t.Fatalf("expected initial route to pin sticky session to pinned-node, got %v", node)
	}

	// Push pinned-node to its cap, then route the same session again.
	atomic.StoreInt32(&pinnedNode.ActiveConns, 1)
	node, _, _ = r.Route("", "sess-1", "")
	if node == pinnedNode {
		t.Error("sticky-session path returned a node at its in-flight cap - capacity must be re-checked even for a pinned session")
	}
}

// TestMaxInFlightRouteExcludingFallsThroughWhenOnlyAlternateOverCap verifies
// RouteExcluding (the proxy retry loop's alternate-node lookup) behaves the
// same as "no eligible node" when the only remaining candidate is over its
// cap - it must not select an over-cap node, and must not loop or panic.
func TestMaxInFlightRouteExcludingFallsThroughWhenOnlyAlternateOverCap(t *testing.T) {
	r := New(config.RoutingConfig{MaxInFlightPerNode: 1}, []config.NodeConfig{
		{Name: "tried-node", URL: "http://tried.invalid", Runtime: "ollama"},
		{Name: "over-cap-node", URL: "http://overcap.invalid", Runtime: "ollama"},
	}, nil)

	var overCapNode *NodeState
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Unlock()
		if n.Name == "over-cap-node" {
			overCapNode = n
		}
	}
	atomic.StoreInt32(&overCapNode.ActiveConns, 1) // at cap

	node, ok, _ := r.RouteExcluding("", "", map[string]bool{"http://tried.invalid": true})
	if node != nil || ok {
		t.Fatalf("expected no eligible alternate (only candidate is over cap), got node=%v ok=%v", node, ok)
	}
}
