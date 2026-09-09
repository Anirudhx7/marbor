package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// pval1_scenarios_test.go implements the router-brain validation gate's
// remaining test scenarios that are runnable today without real vLLM/TGI
// hardware: same-model warm-vs-cold selection, overloaded-node exclusion,
// tensor-parallel GPU-group gating, node-failure redistribution, VRAM-fit
// selection among cold candidates, and mixed-runtime eligibility. A separate
// file already covers the concurrent-load-spread scenario. Two scenarios
// (multi-turn conversation-prefix stickiness and multi-adapter routing) stay
// excluded here - they depend on features not yet shipped. The cloud-overflow
// scenario (cloud fallback only activates when local capacity genuinely
// cannot satisfy the request, never before) is not duplicated here either -
// it is already proven end-to-end at the proxy layer by
// TestProxyFallsBackToCloud and TestProxyNoFallbackWhenCloudDisabled
// (internal/proxy/proxy_test.go), which is the correct layer for it since
// the local-exhausted-before-cloud decision lives in proxy.go, not router.go
// (RouteCloud itself is unconditional - the caller decides when to use it).
//
// This is a proof/verification pass, not a new routing feature - it exercises
// existing, already-shipped mechanisms (warm residency, capacity exclusion,
// GPU-group gating, health-based redistribution, VRAM-fit scoring, per-runtime
// eligibility). No production code changes accompany this file.

// Scenario 1: 4 GPUs (nodes), same model warm on 2 of them -> a warm node
// wins over cold nodes.
func TestPVAL1Scenario1_WarmNodeWinsOverColdNodes(t *testing.T) {
	nodesCfg := []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		{Name: "node-c", URL: "http://node-c:11434", VRAMTotalMB: 8192},
		{Name: "node-d", URL: "http://node-d:11434", VRAMTotalMB: 8192},
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	// node-c and node-d are warm for model-x; node-a and node-b are cold.
	r.nodes[2].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}
	r.nodes[3].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}

	node, warm, decision := r.Route("model-x", "", "")
	if node == nil {
		t.Fatal("expected a node, got nil")
	}
	if !warm {
		t.Errorf("warm = false, want true (a warm node should have won)")
	}
	if node.Name != "node-c" && node.Name != "node-d" {
		t.Errorf("selected node %q, want node-c or node-d (the warm nodes)", node.Name)
	}
	if decision == nil || decision.Reason != ReasonScoreBased {
		t.Errorf("decision.Reason = %v, want score_based", decision)
	}
}

// Scenario 2: a warm node becomes overloaded (at its in-flight cap) -> a
// healthy, under-capacity node wins instead, even though it is cold. This
// is a hard-capacity exclusion (isUnderCapacity), not a scoring-penalty
// contest - the whole point of the eligibility/capacity split already in
// routeInternal is that an overloaded node is removed from candidacy, not
// merely scored lower than a 50-point warm bonus could ever be outweighed by.
func TestPVAL1Scenario2_OverloadedWarmNodeLosesToHealthyLessLoadedNode(t *testing.T) {
	nodesCfg := []config.NodeConfig{
		{Name: "node-warm", URL: "http://node-warm:11434", VRAMTotalMB: 8192},
		{Name: "node-cold", URL: "http://node-cold:11434", VRAMTotalMB: 8192},
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}
	r.nodes[0].MaxInFlight = 1
	r.nodes[0].ActiveConns = 1 // at cap -> isUnderCapacity(node-warm) == false

	node, warm, _ := r.Route("model-x", "", "")
	if node == nil {
		t.Fatal("expected a node, got nil")
	}
	if node.Name != "node-cold" {
		t.Errorf("selected node %q, want node-cold (node-warm is at capacity)", node.Name)
	}
	if warm {
		t.Error("warm = true, want false (node-cold has no copy of model-x)")
	}
}

// Scenario 3: an 8-GPU tensor-parallel model deployment is correctly
// excluded from 4-GPU nodes. isGPUGroupSufficient is the hard filter under
// test - two 4-GPU nodes declare (via AgentGPUs) that they cannot satisfy an
// 8-wide TP requirement, one 8-GPU node can.
func TestPVAL1Scenario3_TensorParallelExcludesUndersizedNodes(t *testing.T) {
	nodesCfg := []config.NodeConfig{
		{Name: "node-4gpu-a", URL: "http://node-4gpu-a:8000", VRAMTotalMB: 8192, Runtime: "vllm"},
		{Name: "node-4gpu-b", URL: "http://node-4gpu-b:8000", VRAMTotalMB: 8192, Runtime: "vllm"},
		{Name: "node-8gpu", URL: "http://node-8gpu:8000", VRAMTotalMB: 8192, Runtime: "vllm"},
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	for i := range r.nodes {
		r.nodes[i].ParallelismWidth = 8
	}
	r.nodes[0].AgentGPUs = fourGPUs()
	r.nodes[1].AgentGPUs = fourGPUs()
	r.nodes[2].AgentGPUs = eightGPUs()
	// vLLM requires the model already warm to be eligible at all - make it
	// warm everywhere so this test isolates the GPU-group filter, not the
	// warm-eligibility filter scenario 9 already covers.
	for i := range r.nodes {
		r.nodes[i].LoadedModels = []ModelInfo{{Name: "big-tp-model", SizeVRAM: 4000}}
	}

	node, _, _ := r.Route("big-tp-model", "", "")
	if node == nil {
		t.Fatal("expected a node, got nil")
	}
	if node.Name != "node-8gpu" {
		t.Errorf("selected node %q, want node-8gpu (the only node with sufficient GPU group)", node.Name)
	}
}

func fourGPUs() []marboragent.GPUInfo {
	return []marboragent.GPUInfo{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}}
}

func eightGPUs() []marboragent.GPUInfo {
	return []marboragent.GPUInfo{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}, {Index: 5}, {Index: 6}, {Index: 7}}
}

// Scenario 6: a node fails mid-fleet -> traffic correctly redistributes to
// the remaining healthy nodes, and never picks the failed one again.
func TestPVAL1Scenario6_NodeFailureRedistributesTraffic(t *testing.T) {
	nodesCfg := []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 8192},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 8192},
		{Name: "node-c", URL: "http://node-c:11434", VRAMTotalMB: 8192},
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)

	node, _, _ := r.Route("model-x", "", "")
	if node == nil {
		t.Fatal("expected a node before any failure, got nil")
	}

	// node-a fails.
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()

	for i := 0; i < 10; i++ {
		node, _, _ := r.Route("model-x", "", "")
		if node == nil {
			t.Fatalf("iteration %d: expected a node, got nil (failure should redistribute, not exhaust the fleet)", i)
		}
		if node.Name == "node-a" {
			t.Fatalf("iteration %d: routed to node-a, which is marked unhealthy", i)
		}
	}
}

// Scenario 7: a cold model request selects the best VRAM-fit node, not just
// any available node - two cold candidates with genuinely different free
// headroom (not an equal-ratio tie, which the existing "All nodes cold"
// placement test already covers) must resolve to the one with more room.
func TestPVAL1Scenario7_ColdRequestPicksBestVRAMFitNotJustAnyNode(t *testing.T) {
	nodesCfg := []config.NodeConfig{
		{Name: "node-tight", URL: "http://node-tight:11434", VRAMTotalMB: 8192},
		{Name: "node-roomy", URL: "http://node-roomy:11434", VRAMTotalMB: 8192},
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	r.nodes[0].VRAMUsedMB = 7000 // node-tight: little free headroom
	r.nodes[1].VRAMUsedMB = 500  // node-roomy: most of its VRAM free

	node, warm, _ := r.Route("model-x", "", "")
	if node == nil {
		t.Fatal("expected a node, got nil")
	}
	if warm {
		t.Error("warm = true, want false (neither node has model-x loaded)")
	}
	if node.Name != "node-roomy" {
		t.Errorf("selected node %q, want node-roomy (genuinely more free VRAM headroom)", node.Name)
	}
}

// Scenario 9: mixed runtimes in one fleet each get correct runtime-specific
// eligibility behavior from the same router - Ollama's on-demand-load
// exemption (isEligibleForModel) vs. vLLM's warm-only requirement.
func TestPVAL1Scenario9_MixedRuntimesGetCorrectEligibilityBehavior(t *testing.T) {
	t.Run("uncommon cold model only eligible on the Ollama node", func(t *testing.T) {
		nodesCfg := []config.NodeConfig{
			{Name: "ollama-node", URL: "http://ollama-node:11434", VRAMTotalMB: 8192, Runtime: "ollama"},
			{Name: "vllm-node", URL: "http://vllm-node:8000", VRAMTotalMB: 8192, Runtime: "vllm"},
		}
		r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
		// Neither node has "rare-model" loaded. vLLM has no on-demand-load
		// path, so it must be excluded entirely, leaving Ollama as the only
		// eligible candidate.
		node, _, _ := r.Route("rare-model", "", "")
		if node == nil {
			t.Fatal("expected a node, got nil")
		}
		if node.Name != "ollama-node" {
			t.Errorf("selected node %q, want ollama-node (vllm-node cannot on-demand-load)", node.Name)
		}
	})

	t.Run("model warm on vllm node wins over cold ollama node", func(t *testing.T) {
		nodesCfg := []config.NodeConfig{
			{Name: "ollama-node", URL: "http://ollama-node:11434", VRAMTotalMB: 8192, Runtime: "ollama"},
			{Name: "vllm-node", URL: "http://vllm-node:8000", VRAMTotalMB: 8192, Runtime: "vllm"},
		}
		r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
		r.nodes[1].LoadedModels = []ModelInfo{{Name: "shared-model", SizeVRAM: 4000}}

		node, warm, _ := r.Route("shared-model", "", "")
		if node == nil {
			t.Fatal("expected a node, got nil")
		}
		if node.Name != "vllm-node" {
			t.Errorf("selected node %q, want vllm-node (warm beats cold Ollama)", node.Name)
		}
		if !warm {
			t.Error("warm = false, want true")
		}
	})
}
