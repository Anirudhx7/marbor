package router

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func TestPlacementScoring(t *testing.T) {
	// Helper to build a clean Router with nodes
	newTestRouter := func(nodesCfg []config.NodeConfig) *Router {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
		return r
	}

	t.Run("Single node only", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 8192},
		})
		node, warm, _ := r.Route("model-x", "", "")
		if node == nil {
			t.Fatal("expected to select a node, got nil")
		}
		if node.Name != "node-a" {
			t.Errorf("selected node %q, want \"node-a\"", node.Name)
		}
		if warm {
			t.Error("expected warm=false, got true")
		}
	})

	t.Run("All nodes cold - pick best by VRAM/conns/tiebreak", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 16384}, // More VRAM -> higher score
		})

		node, warm, _ := r.Route("model-x", "", "")
		if node == nil {
			t.Fatal("expected to select a node, got nil")
		}
		// Node B has more free VRAM headroom (100% of 16GB vs 100% of 8GB - wait, both have 100% free ratio,
		// but wait! free_vram_headroom = free / total, so both have ratio 1.0!
		// But let's check: if both have ratio 1.0, scores are equal, so it tiebreaks to node-a lexicographically!)
		if node.Name != "node-a" {
			t.Errorf("selected node %q, want \"node-a\" (tiebreak)", node.Name)
		}
		if warm {
			t.Error("expected warm=false")
		}

		// Now make node-a have active connections (reduces its score)
		atomic.StoreInt32(&r.nodes[0].ActiveConns, 1) // node-a has 1 conn, node-b has 0 conns
		node, _, _ = r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" due to lower conns on B", node.Name)
		}
	})

	t.Run("All nodes VRAM-full", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		})

		// Make both VRAM full
		r.nodes[0].VRAMUsedMB = 8192
		r.nodes[1].VRAMUsedMB = 8192

		// Both are equally full, should fall back to conns/tiebreak (node-a chosen)
		node, _, _ := r.Route("model-x", "", "")
		if node == nil {
			t.Fatal("expected node, got nil")
		}
		if node.Name != "node-a" {
			t.Errorf("selected node %q, want \"node-a\"", node.Name)
		}
	})

	t.Run("Pinned model on a cold node vs warm unpinned node", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		})

		// Pin model-x on node-b (which is cold)
		r.SetPinnedModels("node-b", []string{"model-x"})

		// Load model-x on node-a (making node-a warm for model-x)
		r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}

		// Pinning model-x on cold node-b shouldn't force routing to B because B is cold.
		// Node A is warm, so Node A should get the +50 warm resident score and win.
		node, warm, _ := r.Route("model-x", "", "")
		if node == nil {
			t.Fatal("expected node, got nil")
		}
		if node.Name != "node-a" {
			t.Errorf("selected node %q, want \"node-a\" (warm node wins over cold pinned)", node.Name)
		}
		if !warm {
			t.Error("expected warm=true")
		}
	})

	t.Run("Pinned model on a warm node", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		})

		// Pin model-x on node-b
		r.SetPinnedModels("node-b", []string{"model-x"})

		// Make BOTH nodes warm
		r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}
		r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}

		// Node B is pinned and warm. It should be selected immediately.
		node, warm, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" (pinned and warm)", node.Name)
		}
		if !warm {
			t.Error("expected warm=true")
		}
	})

	t.Run("Two nodes equally scored (deterministic tiebreak)", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
			{Name: "node-c", URL: "http://node-c:11434", VRAMTotalMB: 8192},
		})

		node, _, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" (lexicographical tiebreak 'b' < 'c')", node.Name)
		}
	})

	t.Run("Node cooldown after upstream error", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		})

		// Make both warm
		r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}
		r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}

		// Trigger cooldown on node-a
		r.RecordRequestOutcome("node-a", false)

		// Node A is in cooldown, so its score is penalized by 50 points.
		// Node B should win.
		node, _, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" (node-a is in cooldown)", node.Name)
		}

		// Clear cooldown by changing LastErrorAt to >60s ago
		r.nodes[0].mu.Lock()
		r.nodes[0].LastErrorAt = time.Now().Add(-70 * time.Second)
		r.nodes[0].SuccessHistory = nil // reset success history so success rate doesn't skew score
		r.nodes[0].mu.Unlock()

		// Node A is out of cooldown, should win via tiebreaker 'a' < 'b'
		node, _, _ = r.Route("model-x", "", "")
		if node.Name != "node-a" {
			t.Errorf("selected node %q, want \"node-a\" (cooldown expired)", node.Name)
		}
	})

	t.Run("Recent success rate tracking", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		})

		// Record successes on node-b, failures on node-a (but clear cooldown so only success rate matters)
		for i := 0; i < 10; i++ {
			r.RecordRequestOutcome("node-b", true)
		}
		for i := 0; i < 5; i++ {
			r.RecordRequestOutcome("node-a", false)
		}
		r.nodes[0].mu.Lock()
		r.nodes[0].LastErrorAt = time.Time{} // Clear cooldown penalty
		r.nodes[0].mu.Unlock()

		// Node B has 100% success rate, Node A has 0%.
		// Node B should score higher (recent_success_rate weight is 5) and win.
		node, _, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" due to higher success rate", node.Name)
		}
	})
}

// TestRoute_ColdStartReservationPreventsDoubleBooking is the cold-start
// reservation regression test. Two identically-scored cold nodes both have just enough VRAM for ONE
// of two same-size models, never both. Without a hot-path reservation, both
// Route calls independently see the same stale (unreserved) snapshot and the
// deterministic tiebreak would send BOTH models to node-a - a real
// double-booking. With the fix, the first cold pick's reservation discounts
// node-a's free_vram_headroom term enough that the second pick's score
// comparison favors node-b instead.
func TestRoute_ColdStartReservationPreventsDoubleBooking(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
		{Name: "node-b", URL: "http://node-b:11434"},
	}, nil)

	// VRAMTotalMB is the live, poll-populated field (unlike NodeConfig's
	// VRAMTotalMB, which only seeds NodeState.VRAMTotalMBConfig) - set it
	// directly, matching every other placement test in this file.
	r.nodes[0].mu.Lock()
	r.nodes[0].VRAMTotalMB = 5000
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].VRAMTotalMB = 5000
	r.nodes[1].mu.Unlock()

	// Both models' real VRAM footprint is already known from a prior
	// observation (the common case in practice) - this is exactly the
	// zero-I/O data reserveColdStartBytes is allowed to use on the hot path.
	r.recordLastKnownVRAM("node-a", "model-a", 3000*mib)
	r.recordLastKnownVRAM("node-b", "model-a", 3000*mib)
	r.recordLastKnownVRAM("node-a", "model-b", 3000*mib)
	r.recordLastKnownVRAM("node-b", "model-b", 3000*mib)

	// First cold request: node-a and node-b tie on every score term, so the
	// deterministic alphabetical tiebreak picks node-a.
	n1, warm1, _ := r.Route("model-a", "", "")
	if n1 == nil || n1.Name != "node-a" {
		t.Fatalf("first Route(model-a) = %v, want node-a (tiebreak)", n1)
	}
	if warm1 {
		t.Fatal("first Route(model-a) reported warm=true, want false (cold)")
	}

	// Second, concurrent-in-spirit request for a DIFFERENT cold model: node-a
	// only has 5000-3000=2000 MiB left once model-a's reservation is
	// discounted, not enough headroom to also justify picking it over node-b
	// (which is still fully free). node-b must win - proving the two models
	// did not both land on node-a.
	n2, warm2, _ := r.Route("model-b", "", "")
	if n2 == nil || n2.Name != "node-b" {
		t.Fatalf("second Route(model-b) = %v, want node-b (node-a's headroom must be discounted by model-a's pending reservation)", n2)
	}
	if warm2 {
		t.Fatal("second Route(model-b) reported warm=true, want false (cold)")
	}
}

// TestRoute_ColdStartReservationPreventsDoubleBooking_UnknownSize covers the
// unknown-size cold-start reservation fix:
// reserveColdStartBytes used to be a no-op when a model's size couldn't be
// determined (no lastKnownVRAM, no override, no allowed fetch on this hot
// path), so a burst of concurrent cold starts for a never-seen model all read
// the same "fully free" snapshot and could all land on the same node. Unlike
// TestRoute_ColdStartReservationPreventsDoubleBooking above, this test
// deliberately records NO lastKnownVRAM for either node/model, so
// estimateModelSizeBytes returns 0 and the fix must fall back to
// unknownModelReserveBytes rather than reserving nothing.
func TestRoute_ColdStartReservationPreventsDoubleBooking_UnknownSize(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
		{Name: "node-b", URL: "http://node-b:11434"},
	}, nil)

	r.nodes[0].mu.Lock()
	r.nodes[0].VRAMTotalMB = 5000
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].VRAMTotalMB = 5000
	r.nodes[1].mu.Unlock()

	// First cold request for a never-seen model: node-a and node-b tie on
	// every score term, so the deterministic alphabetical tiebreak picks
	// node-a.
	n1, warm1, _ := r.Route("model-unknown-a", "", "")
	if n1 == nil || n1.Name != "node-a" {
		t.Fatalf("first Route(model-unknown-a) = %v, want node-a (tiebreak)", n1)
	}
	if warm1 {
		t.Fatal("first Route(model-unknown-a) reported warm=true, want false (cold)")
	}

	// Second, concurrent-in-spirit request for a DIFFERENT never-seen model:
	// before this fix, reserveColdStartBytes was a no-op for an
	// unknown-size model, so node-a's headroom would look untouched and this
	// second request would also tiebreak onto node-a, double-booking it. The
	// fix's placeholder reservation must discount node-a's headroom so
	// node-b - still fully free - wins instead.
	n2, warm2, _ := r.Route("model-unknown-b", "", "")
	if n2 == nil || n2.Name != "node-b" {
		t.Fatalf("second Route(model-unknown-b) = %v, want node-b (node-a's headroom must be discounted by model-unknown-a's placeholder reservation)", n2)
	}
	if warm2 {
		t.Fatal("second Route(model-unknown-b) reported warm=true, want false (cold)")
	}
}

// TestIsModelWarm_DigestMismatchNotWarm covers the case where two nodes
// serve the same model NAME with different content digests (a stale
// re-pull, a mismatched quantization) - they must not be treated as
// interchangeable warm hits.
func TestIsModelWarm_DigestMismatchNotWarm(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
	}, nil)

	// node-a's copy is observed first and becomes the recorded reference digest.
	r.recordModelDigest("model-x", "sha256:aaa")
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:aaa"}}
	// node-b has a DIFFERENT digest under the same name - e.g. a re-pull that
	// landed a different quantization without renaming the tag.
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:bbb"}}

	if !r.isModelWarm(r.nodes[0], "model-x") {
		t.Error("node-a: isModelWarm = false, want true (digest matches the recorded reference)")
	}
	if r.isModelWarm(r.nodes[1], "model-x") {
		t.Error("node-b: isModelWarm = true, want false (digest conflicts with node-a's recorded reference)")
	}

	// computeNodeScore must not award the +50 warm bonus to the mismatched node either.
	scoreA := r.computeNodeScore(r.nodes[0], "model-x")
	scoreB := r.computeNodeScore(r.nodes[1], "model-x")
	if scoreA <= scoreB {
		t.Errorf("scoreA=%v scoreB=%v, want scoreA > scoreB (only node-a's digest-matched copy should get the warm bonus)", scoreA, scoreB)
	}
}

// TestIsModelWarm_NonOllamaQuantVariantsNotFungible covers the case where a vLLM Q4 build
// and a vLLM F16 build of "llama-3-70b" served under the identical name on
// two different nodes must not be treated as interchangeable warm hits, now
// that internal/runtime/vllm.go populates Digest from vLLM's own reported
// "root" (local model path) field when it differs from the served name.
// This reuses the exact same digestMismatch/isModelWarm machinery already
// covered by TestIsModelWarm_DigestMismatchNotWarm - the fix is entirely in
// how the Digest field gets populated upstream (internal/runtime probes),
// not a change to this comparison logic itself.
func TestIsModelWarm_NonOllamaQuantVariantsNotFungible(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:8000", VRAMTotalMB: 81920},
		{Name: "node-b", URL: "http://node-b:8000", VRAMTotalMB: 81920},
	}, nil)

	r.recordModelDigest("llama-3-70b", "/models/llama-3-70b-f16")
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama-3-70b", Digest: "/models/llama-3-70b-f16"}}
	// node-b serves a Q4 quantization under the identical model name -
	// vLLM's own "root" field is the only signal that distinguishes them.
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "llama-3-70b", Digest: "/models/llama-3-70b-q4_k_m"}}

	if !r.isModelWarm(r.nodes[0], "llama-3-70b") {
		t.Error("node-a (F16): isModelWarm = false, want true (matches recorded reference)")
	}
	if r.isModelWarm(r.nodes[1], "llama-3-70b") {
		t.Error("node-b (Q4): isModelWarm = true, want false - a Q4 and an F16 build under the same name must never be treated as fungible warm")
	}
}

// TestIsModelWarm_MissingDigestNeverFlagged covers the digest check's honesty
// requirement: a runtime that doesn't report a digest (anything but Ollama today) must
// never be treated as mismatched just because it reported nothing.
func TestIsModelWarm_MissingDigestNeverFlagged(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
	}, nil)

	r.recordModelDigest("model-x", "sha256:aaa")
	// No digest reported for this node's copy (e.g. vLLM/TGI/llama.cpp/MLX).
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x"}}

	if !r.isModelWarm(r.nodes[0], "model-x") {
		t.Error("isModelWarm = false, want true (missing digest must never be treated as a mismatch)")
	}
}

// TestReconcileModelDigests_FleetConvergesOnNewDigest is the fleet-convergence
// regression test: once the entire fleet has moved on to a new digest for a model name (e.g.
// every node re-pulled it), the stored reference digest must follow, so
// isModelWarm stops permanently flagging every node as mismatched.
func TestReconcileModelDigests_FleetConvergesOnNewDigest(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
	}, nil)

	r.recordModelDigest("model-x", "sha256:aaa")
	// The whole fleet has since re-pulled and moved on to a new digest - no
	// node reports the old one anymore.
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:bbb"}}
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:bbb"}}

	if r.isModelWarm(r.nodes[0], "model-x") {
		t.Fatal("precondition: expected isModelWarm=false before reconciliation")
	}

	r.reconcileModelDigests(r.nodes)

	if !r.isModelWarm(r.nodes[0], "model-x") {
		t.Error("node-a: isModelWarm = false after reconciliation, want true (fleet converged on the new digest)")
	}
	if !r.isModelWarm(r.nodes[1], "model-x") {
		t.Error("node-b: isModelWarm = false after reconciliation, want true (fleet converged on the new digest)")
	}
}

// TestReconcileModelDigests_PartialMigrationKeepsOldReference: while only
// SOME nodes have moved to a new digest and at least one node still reports
// the old one, the reference digest must NOT flip - flipping mid-migration
// would flag the not-yet-migrated node as mismatched instead.
func TestReconcileModelDigests_PartialMigrationKeepsOldReference(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
	}, nil)

	r.recordModelDigest("model-x", "sha256:aaa")
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:aaa"}}
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x", Digest: "sha256:bbb"}}

	r.reconcileModelDigests(r.nodes)

	if !r.isModelWarm(r.nodes[0], "model-x") {
		t.Error("node-a: isModelWarm = false, want true (old reference digest must be preserved mid-migration)")
	}
	if r.isModelWarm(r.nodes[1], "model-x") {
		t.Error("node-b: isModelWarm = true, want false (still genuinely mismatched against the preserved reference)")
	}
}

// TestScoreComponentsSumEqualsComputeNodeScore is a hard acceptance
// criterion: the exposed component breakdown must sum to exactly the score
// computeNodeScore actually used to pick a winner, for both unpenalized and
// penalized nodes (where the floor-at-zero clamp means a penalty's Value is
// not always the nominal -50).
func TestScoreComponentsSumEqualsComputeNodeScore(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
	}, nil)

	t.Run("unpenalized", func(t *testing.T) {
		want := r.computeNodeScore(r.nodes[0], "model-x")
		got := sumComponents(r.scoreComponents(r.nodes[0], "model-x"))
		if got != want {
			t.Errorf("sum(components) = %v, want %v (must equal computeNodeScore exactly)", got, want)
		}
	})

	t.Run("cooldown penalty clamped at zero", func(t *testing.T) {
		// Drive the running score to nearly zero so the -50 cooldown penalty
		// gets clamped, making its actual Value != -50.
		r.nodes[0].VRAMUsedMB = 8192 // zero free-VRAM headroom
		atomic.StoreInt32(&r.nodes[0].ActiveConns, 1000)
		r.nodes[0].HealthHistory = []float64{0}
		r.nodes[0].SuccessHistory = []bool{false}
		r.nodes[0].LastErrorAt = time.Now()

		want := r.computeNodeScore(r.nodes[0], "model-x")
		components := r.scoreComponents(r.nodes[0], "model-x")
		got := sumComponents(components)
		if got != want {
			t.Errorf("sum(components) = %v, want %v (penalty must be the actual clamped delta)", got, want)
		}
		found := false
		for _, c := range components {
			if c.Name == "cooldown_penalty" {
				found = true
				if c.Value == -50.0 {
					t.Error("cooldown_penalty.Value = -50 exactly; this case is only meaningful if the floor clamped it to something less negative")
				}
			}
		}
		if !found {
			t.Fatal("no cooldown_penalty component present")
		}
	})

	t.Run("both penalties triggered", func(t *testing.T) {
		r2 := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 1000, HealthFailureThreshold: 3}, []config.NodeConfig{
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		}, nil)
		r2.nodes[0].LastErrorAt = time.Now()
		r2.nodes[0].LastPollAt = time.Now().Add(-time.Hour)

		want := r2.computeNodeScore(r2.nodes[0], "model-x")
		components := r2.scoreComponents(r2.nodes[0], "model-x")
		got := sumComponents(components)
		if got != want {
			t.Errorf("sum(components) = %v, want %v", got, want)
		}
		// Raw == 1.0 signals "triggered" regardless of Value, which may be
		// clamped to 0 if an earlier penalty already zeroed the running
		// score - both penalties can be triggered while only one has a
		// nonzero Value, and that's the correct, real behavior to assert.
		var sawCooldown, sawStale bool
		for _, c := range components {
			if c.Name == "cooldown_penalty" && c.Raw == 1.0 {
				sawCooldown = true
			}
			if c.Name == "stale_telemetry_penalty" && c.Raw == 1.0 {
				sawStale = true
			}
		}
		if !sawCooldown || !sawStale {
			t.Errorf("expected both penalties triggered, sawCooldown=%v sawStale=%v", sawCooldown, sawStale)
		}
	})
}

// TestRouteDecisionReasons covers the three RoutingDecision.Reason values
// plus AffinityLost, as part of the routing-path audit.
func TestRouteDecisionReasons(t *testing.T) {
	t.Run("session_affinity", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first", SessionAffinity: true}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		}, nil)

		node1, _, d1 := r.Route("model-x", "sess-1", "")
		if node1 == nil {
			t.Fatal("expected a node on first call")
		}
		if d1.Reason != ReasonScoreBased {
			t.Errorf("first call: Reason = %q, want %q (no affinity entry exists yet, this is the initial pick)", d1.Reason, ReasonScoreBased)
		}

		node2, _, d2 := r.Route("model-x", "sess-1", "")
		if node2 == nil || node2.Name != node1.Name {
			t.Fatalf("expected sticky session to return node %q again, got %v", node1.Name, node2)
		}
		if d2.Reason != ReasonSessionAffinity {
			t.Errorf("Reason = %q, want %q", d2.Reason, ReasonSessionAffinity)
		}
		if d2.AffinityLost {
			t.Error("AffinityLost = true on a successful sticky hit, want false")
		}
	})

	t.Run("pinned_warm", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		}, nil)
		r.SetPinnedModels("node-b", []string{"model-x"})
		r.nodes[1].LoadedModels = []ModelInfo{{Name: "model-x"}}

		node, _, decision := r.Route("model-x", "", "")
		if node == nil || node.Name != "node-b" {
			t.Fatalf("expected node-b (pinned+warm), got %v", node)
		}
		if decision.Reason != ReasonPinnedWarm {
			t.Errorf("Reason = %q, want %q", decision.Reason, ReasonPinnedWarm)
		}
		if len(decision.Components) != 0 {
			t.Error("pinned_warm decision should not carry a score breakdown (scoring was short-circuited)")
		}
	})

	t.Run("score_based with component breakdown", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		}, nil)

		node, _, decision := r.Route("model-x", "", "")
		if node == nil {
			t.Fatal("expected a node")
		}
		if decision.Reason != ReasonScoreBased {
			t.Errorf("Reason = %q, want %q", decision.Reason, ReasonScoreBased)
		}
		if len(decision.Components) == 0 {
			t.Fatal("expected a non-empty component breakdown for a score_based decision")
		}
		if sum := sumComponents(decision.Components); sum != decision.Score {
			t.Errorf("sum(Components) = %v, Score = %v - must be exactly equal", sum, decision.Score)
		}
	})

	t.Run("affinity lost - sticky node unhealthy, falls back to scoring", func(t *testing.T) {
		r := New(config.RoutingConfig{Strategy: "warm-first", SessionAffinity: true}, []config.NodeConfig{
			{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
			{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		}, nil)

		node1, _, _ := r.Route("model-x", "sess-1", "")
		if node1 == nil {
			t.Fatal("expected a node on first call")
		}
		// Mark the sticky node unhealthy so the second call must fall through.
		node1.mu.Lock()
		node1.Healthy = false
		node1.mu.Unlock()

		node2, _, decision := r.Route("model-x", "sess-1", "")
		if node2 == nil {
			t.Fatal("expected fallback routing to still return a node")
		}
		if !decision.AffinityLost {
			t.Error("AffinityLost = false, want true (session had an entry that failed validation)")
		}
		if decision.Reason == ReasonSessionAffinity {
			t.Error("Reason should not be session_affinity once the sticky node failed validation")
		}
	})
}

// TestPlacementScoring_TTFTWeightedLoad covers the TTFT-weighted load case: a node serving a few
// prefill-heavy connections (slow real observed TTFT) must score worse than
// a node serving many decode-light connections (fast TTFT), even though the
// heavy node has the lower raw ActiveConns count. Before the fix, both nodes
// only differed by float64(conns), so node-a (fewer conns) always won.
func TestPlacementScoring_TTFTWeightedLoad(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
	}, nil)

	nodeA, nodeB := r.nodes[0], r.nodes[1]

	// node-a: 2 heavy prefill connections, slow real observed TTFT (large
	// prompts keep the node's compute busy for seconds before first byte).
	atomic.StoreInt32(&nodeA.ActiveConns, 2)
	nodeA.RecentTTFT = []float64{3.5, 4.0, 4.5}

	// node-b: 8 light decode-only continuations, fast real observed TTFT.
	atomic.StoreInt32(&nodeB.ActiveConns, 8)
	nodeB.RecentTTFT = []float64{0.08, 0.1, 0.09}

	scoreA := r.computeNodeScore(nodeA, "model-x")
	scoreB := r.computeNodeScore(nodeB, "model-x")
	if scoreA >= scoreB {
		t.Errorf("node-a (2 heavy prefill conns) score=%v, node-b (8 light decode conns) score=%v; want node-a < node-b", scoreA, scoreB)
	}

	node, _, _ := r.Route("model-x", "", "")
	if node == nil {
		t.Fatal("expected to select a node, got nil")
	}
	if node.Name != "node-b" {
		t.Errorf("selected node %q, want \"node-b\" (lighter effective load despite higher raw conns)", node.Name)
	}

	// No TTFT history yet -> effectiveLoad falls back to the raw conns count
	// unchanged, matching the original unweighted formula exactly (backward compatible).
	nodeC := &NodeState{Name: "node-c", ActiveConns: 3}
	if got := effectiveLoad(nodeC, atomic.LoadInt32(&nodeC.ActiveConns)); got != 3.0 {
		t.Errorf("effectiveLoad with no RecentTTFT = %v, want 3.0 (raw conns, unweighted)", got)
	}
}
