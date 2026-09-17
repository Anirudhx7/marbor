package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestScoreComponents_PhaseLabelsAreParityPreserving pins the known-good
// values already produced by scoreComponents/findBestByScore (winner, total
// Score, and every Name/Raw/Weight/Value) and asserts the phase-labeled
// version matches on all of them, with Phase being the only new field. This
// is not a second scoring implementation to diff against - there is exactly
// one scoreComponents, and this test proves adding Phase changed nothing
// about the arithmetic or the winner.
func TestScoreComponents_PhaseLabelsAreParityPreserving(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
		{Name: "node-b", URL: "http://node-b:11434"},
	}, nil)
	// NodeConfig.VRAMTotalMB only seeds NodeState.VRAMTotalMBConfig, not the
	// live poll-populated VRAMTotalMB field scoreComponents reads - set it
	// directly, matching the pattern in TestRoute_ColdStartReservationPreventsDoubleBooking.
	r.nodes[0].mu.Lock()
	r.nodes[0].VRAMTotalMB = 8192
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].VRAMTotalMB = 8192
	r.nodes[1].mu.Unlock()

	node, components := r.findBestByScore(r.nodes, "model-x")
	if node == nil {
		t.Fatal("expected a winning node")
	}
	if node.Name != "node-a" {
		t.Fatalf("winner = %q, want %q (unchanged tiebreak behavior)", node.Name, "node-a")
	}

	// sumComponents == computeNodeScore is already covered independently by
	// TestScoreComponentsSumEqualsComputeNodeScore; not re-asserted here
	// since findBestByScore's cold-start reservation side effect (this
	// model isn't warm) would make a second computeNodeScore call see
	// discounted headroom and not match the captured components.
	if got, want := sumComponents(components), 50.0; got != want {
		t.Fatalf("sumComponents = %v, want %v", got, want)
	}

	wantNames := []struct {
		Name          string
		Raw           float64
		Weight, Value float64
		Phase         string
	}{
		{"warm_model_resident", 0, 50.0, 0, PhaseLocality},
		{"free_vram_headroom", 1.0, 20.0, 20.0, PhaseLocality},
		{"inverse_queue_depth", 1.0, 15.0, 15.0, PhasePredictedPerformance},
		{"node_health", 1.0, 10.0, 10.0, PhaseReliability},
		{"success_rate", 1.0, 5.0, 5.0, PhaseReliability},
		{"cooldown_penalty", 0, -50.0, 0, PhaseReliability},
		{"stale_telemetry_penalty", 0, -50.0, 0, PhaseReliability},
	}
	if len(components) != len(wantNames) {
		t.Fatalf("got %d components, want %d", len(components), len(wantNames))
	}
	for i, want := range wantNames {
		got := components[i]
		if got.Name != want.Name || got.Raw != want.Raw || got.Weight != want.Weight || got.Value != want.Value {
			t.Errorf("component[%d] = %+v, want Name/Raw/Weight/Value = %q/%v/%v/%v", i, got, want.Name, want.Raw, want.Weight, want.Value)
		}
		if got.Phase != want.Phase {
			t.Errorf("component[%d] %q: Phase = %q, want %q", i, got.Name, got.Phase, want.Phase)
		}
	}
}

// TestFilterCandidates_NoExclusionsWhenAllEligible covers the common case:
// every node survives the pre-score filter, so Excluded/ExcludedTotal must
// both be zero-valued.
func TestFilterCandidates_NoExclusionsWhenAllEligible(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
	}, nil)

	healthy, excluded, excludedTotal := r.filterCandidates(r.nodes, "model-x", "", nil)
	if len(healthy) != 2 {
		t.Fatalf("healthy = %d nodes, want 2", len(healthy))
	}
	if len(excluded) != 0 || excludedTotal != 0 {
		t.Errorf("excluded = %v, excludedTotal = %d, want empty/zero", excluded, excludedTotal)
	}

	_, _, decision := r.Route("model-x", "", "")
	if decision == nil {
		t.Fatal("expected a decision")
	}
	if len(decision.Excluded) != 0 || decision.ExcludedTotal != 0 {
		t.Errorf("decision.Excluded = %v, ExcludedTotal = %d, want empty/zero", decision.Excluded, decision.ExcludedTotal)
	}
}

// TestFilterCandidates_ReasonsMatchFailingCondition covers each of the six
// exclusion reasons, reusing the same conditions the existing eligibility/
// capacity/GPU-group tests exercise (isEligibleForModel, isUnderCapacity,
// isGPUGroupSufficient), asserting the exact reason recorded for each.
func TestFilterCandidates_ReasonsMatchFailingCondition(t *testing.T) {
	t.Run("unhealthy", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		}, nil)
		r.nodes[0].mu.Lock()
		r.nodes[0].Healthy = false
		r.nodes[0].mu.Unlock()

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonUnhealthy)
	})

	t.Run("draining", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		}, nil)
		r.nodes[0].mu.Lock()
		r.nodes[0].Draining = true
		r.nodes[0].mu.Unlock()

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonDraining)
	})

	t.Run("runtime_mismatch", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192, Runtime: "ollama"},
		}, nil)

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "vllm", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonRuntimeMismatch)
	})

	t.Run("ineligible_model", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192, Runtime: "vllm"},
		}, nil)
		// vLLM has no on-demand load path, so it must already report
		// modelName in LoadedModels or isEligibleForModel returns false.

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonIneligibleModel)
	})

	t.Run("over_capacity", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		}, nil)
		r.nodes[0].mu.Lock()
		r.nodes[0].MaxInFlight = 1
		r.nodes[0].mu.Unlock()
		r.nodes[0].ActiveConns = 1 // at cap

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonOverCapacity)
	})

	t.Run("insufficient_gpu_group", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		}, nil)
		r.nodes[0].mu.Lock()
		r.nodes[0].DeclaredGPUIndices = []int{0, 1} // only 2 declared
		r.nodes[0].ParallelismWidth = 4             // but 4 required
		r.nodes[0].mu.Unlock()

		_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
		mustExcludeOne(t, excluded, total, "node-a", ExcludeReasonInsufficientGPUGroup)
	})
}

// TestFilterCandidates_CapsExcludedListAndReportsTotal covers the
// maxExcludedCandidates(20) truncation: with 25 unhealthy nodes, Excluded
// must carry exactly 20 entries and ExcludedTotal must report 25.
func TestFilterCandidates_CapsExcludedListAndReportsTotal(t *testing.T) {
	var nodesCfg []config.NodeConfig
	for i := 0; i < 25; i++ {
		nodesCfg = append(nodesCfg, config.NodeConfig{
			Name: "node-" + string(rune('a'+i)), URL: "http://node-" + string(rune('a'+i)) + ":11434", VRAMTotalMB: 8192,
		})
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = false
		n.mu.Unlock()
	}

	_, excluded, total := r.filterCandidates(r.nodes, "model-x", "", nil)
	if len(excluded) != maxExcludedCandidates {
		t.Fatalf("len(excluded) = %d, want %d (capped)", len(excluded), maxExcludedCandidates)
	}
	if total != 25 {
		t.Fatalf("excludedTotal = %d, want 25", total)
	}

	_, _, decision := r.Route("model-x", "", "")
	if decision != nil {
		t.Fatal("expected nil decision - no candidates survived the filter")
	}
}

func mustExcludeOne(t *testing.T, excluded []ExcludedCandidate, total int, wantNode, wantReason string) {
	t.Helper()
	if total != 1 {
		t.Fatalf("excludedTotal = %d, want 1", total)
	}
	if len(excluded) != 1 {
		t.Fatalf("len(excluded) = %d, want 1", len(excluded))
	}
	if excluded[0].Node != wantNode {
		t.Errorf("excluded[0].Node = %q, want %q", excluded[0].Node, wantNode)
	}
	if excluded[0].Reason != wantReason {
		t.Errorf("excluded[0].Reason = %q, want %q", excluded[0].Reason, wantReason)
	}
}
