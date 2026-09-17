package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

// newReplicaTestNode builds a minimal *NodeState for resolveSchedulingRoles
// tests. peers may be nil (no declaration).
func newReplicaTestNode(name string, peers *store.ReplicaPeers) *NodeState {
	return &NodeState{Name: name, ReplicaPeers: peers}
}

func peers(head string, members ...string) *store.ReplicaPeers {
	return &store.ReplicaPeers{Members: members, Head: head}
}

// TestResolveSchedulingRoles_AdversarialMatrix implements the 10-case
// adversarial test matrix required before this feature could be considered
// correctly implemented - a fleet-global, order-independent
// connected-component closure over declared replica_peers edges.
func TestResolveSchedulingRoles_AdversarialMatrix(t *testing.T) {
	t.Run("case1_simple_valid_pair", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", peers("A", "A", "B"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b})
		if roles["A"] != RoleHead {
			t.Errorf("A = %v, want RoleHead", roles["A"])
		}
		if roles["B"] != RoleWorker {
			t.Errorf("B = %v, want RoleWorker", roles["B"])
		}
	})

	t.Run("case2_one_sided_declaration", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", nil) // B exists but declares nothing
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b})
		if roles["A"] != RoleUnresolved {
			t.Errorf("A = %v, want RoleUnresolved", roles["A"])
		}
		if roles["B"] != RoleUnresolved {
			t.Errorf("B = %v, want RoleUnresolved (must never resolve RoleStandalone)", roles["B"])
		}
	})

	t.Run("case3_transitive_chain_order_independent", func(t *testing.T) {
		// A -> [A,B], B -> [B,C], C -> [B,C]
		mk := func() []*NodeState {
			a := newReplicaTestNode("A", peers("", "A", "B"))
			b := newReplicaTestNode("B", peers("", "B", "C"))
			c := newReplicaTestNode("C", peers("", "B", "C"))
			return []*NodeState{a, b, c}
		}
		r := &Router{}

		nodesForward := mk()
		rolesForward := r.resolveSchedulingRoles(nodesForward)

		nodesReverse := []*NodeState{}
		src := mk()
		for i := len(src) - 1; i >= 0; i-- {
			nodesReverse = append(nodesReverse, src[i])
		}
		rolesReverse := r.resolveSchedulingRoles(nodesReverse)

		for _, name := range []string{"A", "B", "C"} {
			if rolesForward[name] != RoleUnresolved {
				t.Errorf("forward order: %s = %v, want RoleUnresolved", name, rolesForward[name])
			}
			if rolesReverse[name] != RoleUnresolved {
				t.Errorf("reverse order: %s = %v, want RoleUnresolved", name, rolesReverse[name])
			}
			if rolesForward[name] != rolesReverse[name] {
				t.Errorf("order-dependence detected: %s forward=%v reverse=%v", name, rolesForward[name], rolesReverse[name])
			}
		}
	})

	t.Run("case4_component_size_mismatch", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("", "A", "B"))
		b := newReplicaTestNode("B", peers("", "A", "B", "C"))
		c := newReplicaTestNode("C", peers("", "A", "B", "C"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b, c})
		for _, name := range []string{"A", "B", "C"} {
			if roles[name] != RoleUnresolved {
				t.Errorf("%s = %v, want RoleUnresolved (component size mismatch)", name, roles[name])
			}
		}
	})

	t.Run("case5_unrelated_standalone_node_absent", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", peers("A", "A", "B"))
		c := newReplicaTestNode("C", nil) // declares nothing, named by nobody
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b, c})
		if roles["A"] != RoleHead {
			t.Errorf("A = %v, want RoleHead", roles["A"])
		}
		if roles["B"] != RoleWorker {
			t.Errorf("B = %v, want RoleWorker", roles["B"])
		}
		if _, present := roles["C"]; present {
			t.Errorf("C present in role map with %v, want absent (-> RoleStandalone)", roles["C"])
		}
	})

	t.Run("case6_missing_peer_fails_closed", func(t *testing.T) {
		// B does not exist as a NodeState in this fleet at all.
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a})
		if roles["A"] != RoleUnresolved {
			t.Errorf("A = %v, want RoleUnresolved (missing peer must fail closed, never RoleHead)", roles["A"])
		}
		if _, present := roles["B"]; present {
			t.Errorf("B present in role map, want absent (not a real node)")
		}
	})

	t.Run("case7_declarer_omits_self", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "B")) // A omits itself from its own declared members
		b := newReplicaTestNode("B", peers("A", "A", "B"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b})
		if roles["A"] != RoleUnresolved {
			t.Errorf("A = %v, want RoleUnresolved (declared set doesn't match component)", roles["A"])
		}
		if roles["B"] != RoleUnresolved {
			t.Errorf("B = %v, want RoleUnresolved", roles["B"])
		}
	})

	t.Run("case8_conflicting_head", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", peers("B", "A", "B"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b})
		if roles["A"] != RoleUnresolved {
			t.Errorf("A = %v, want RoleUnresolved (conflicting head)", roles["A"])
		}
		if roles["B"] != RoleUnresolved {
			t.Errorf("B = %v, want RoleUnresolved", roles["B"])
		}
	})

	t.Run("case9_valid_and_invalid_components_independent", func(t *testing.T) {
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", peers("A", "A", "B"))
		c := newReplicaTestNode("C", peers("C", "C", "D"))
		d := newReplicaTestNode("D", peers("C", "C", "E"))
		e := newReplicaTestNode("E", peers("C", "C", "E"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b, c, d, e})
		if roles["A"] != RoleHead {
			t.Errorf("A = %v, want RoleHead", roles["A"])
		}
		if roles["B"] != RoleWorker {
			t.Errorf("B = %v, want RoleWorker", roles["B"])
		}
		for _, name := range []string{"C", "D", "E"} {
			if roles[name] != RoleUnresolved {
				t.Errorf("%s = %v, want RoleUnresolved", name, roles[name])
			}
		}
	})

	t.Run("case10_multihop_closure_links_two_pairs", func(t *testing.T) {
		// A -> [A,B] head A, B -> [A,B] head A (otherwise valid pair)
		// C -> [C,D] head C, but D -> [C,D,A] head C (D wrongly also names A,
		// linking the two otherwise-separate pairs into one component)
		a := newReplicaTestNode("A", peers("A", "A", "B"))
		b := newReplicaTestNode("B", peers("A", "A", "B"))
		c := newReplicaTestNode("C", peers("C", "C", "D"))
		d := newReplicaTestNode("D", peers("C", "C", "D", "A"))
		r := &Router{}
		roles := r.resolveSchedulingRoles([]*NodeState{a, b, c, d})
		for _, name := range []string{"A", "B", "C", "D"} {
			if roles[name] != RoleUnresolved {
				t.Errorf("%s = %v, want RoleUnresolved (multi-hop closure must catch this)", name, roles[name])
			}
		}
	})
}

// TestResolveSchedulingRoles_NoDeclarations verifies the fast path: a fleet
// with no replica_peers declarations at all returns a nil map, and every
// node is treated as RoleStandalone (absent from the map).
func TestResolveSchedulingRoles_NoDeclarations(t *testing.T) {
	a := newReplicaTestNode("A", nil)
	b := newReplicaTestNode("B", nil)
	r := &Router{}
	roles := r.resolveSchedulingRoles([]*NodeState{a, b})
	if roles != nil {
		t.Errorf("roles = %v, want nil (fast path)", roles)
	}
}

// TestFilterCandidates_ReplicaWorkerExcluded verifies filterCandidates
// excludes a confirmed RoleWorker with ExcludeReasonReplicaWorker, and a
// RoleUnresolved node with ExcludeReasonReplicaUnresolved, while leaving an
// unrelated healthy standalone node unaffected. Also verifies existing
// exclusion reasons still win precedence over the new replica checks for a
// node that fails both.
func TestFilterCandidates_ReplicaWorkerExcluded(t *testing.T) {
	r := &Router{}

	head := &NodeState{Name: "head", URL: "http://head:11434", Healthy: true, ReplicaPeers: peers("head", "head", "worker")}
	worker := &NodeState{Name: "worker", URL: "http://worker:11434", Healthy: true, ReplicaPeers: peers("head", "head", "worker")}
	unresolvedA := &NodeState{Name: "unresolved-a", URL: "http://ua:11434", Healthy: true, ReplicaPeers: peers("unresolved-a", "unresolved-a", "ghost")} // names a nonexistent node -> fails closed
	standalone := &NodeState{Name: "standalone", URL: "http://standalone:11434", Healthy: true}

	nodes := []*NodeState{head, worker, unresolvedA, standalone}
	healthy, excluded, _ := r.filterCandidates(nodes, "", "", nil)

	healthyNames := map[string]bool{}
	for _, n := range healthy {
		healthyNames[n.Name] = true
	}
	if !healthyNames["head"] {
		t.Error("head should remain a candidate (RoleHead)")
	}
	if !healthyNames["standalone"] {
		t.Error("standalone should remain a candidate")
	}
	if healthyNames["worker"] {
		t.Error("worker must be excluded (RoleWorker)")
	}
	if healthyNames["unresolved-a"] {
		t.Error("unresolved-a must be excluded (RoleUnresolved)")
	}

	reasonFor := map[string]string{}
	for _, ex := range excluded {
		reasonFor[ex.Node] = ex.Reason
	}
	if reasonFor["worker"] != ExcludeReasonReplicaWorker {
		t.Errorf("worker exclude reason = %q, want %q", reasonFor["worker"], ExcludeReasonReplicaWorker)
	}
	if reasonFor["unresolved-a"] != ExcludeReasonReplicaUnresolved {
		t.Errorf("unresolved-a exclude reason = %q, want %q", reasonFor["unresolved-a"], ExcludeReasonReplicaUnresolved)
	}
}

// TestFilterCandidates_ExistingReasonsWinPrecedence verifies that a node
// failing BOTH an existing hard-filter check (unhealthy) and the new
// replica-worker check is still recorded under the existing reason - the
// two new cases are appended after the existing chain, never reordered
// ahead of it.
func TestFilterCandidates_ExistingReasonsWinPrecedence(t *testing.T) {
	r := &Router{}
	worker := &NodeState{Name: "worker", URL: "http://worker:11434", Healthy: false, ReplicaPeers: peers("head", "head", "worker")} // also fails the existing health check
	head := &NodeState{Name: "head", URL: "http://head:11434", Healthy: true, ReplicaPeers: peers("head", "head", "worker")}

	_, excluded, _ := r.filterCandidates([]*NodeState{worker, head}, "", "", nil)
	for _, ex := range excluded {
		if ex.Node == "worker" && ex.Reason != ExcludeReasonUnhealthy {
			t.Errorf("worker exclude reason = %q, want %q (existing check must win precedence)", ex.Reason, ExcludeReasonUnhealthy)
		}
	}
}
