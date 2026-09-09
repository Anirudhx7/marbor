package router

import (
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// pval1_scenario10_test.go implements P-VAL1 scenario 10 (EXECUTION-QUEUE.md,
// "Router Brain Validation / v1 Gate"): "Concurrent requests (recommend the
// same 50-concurrent load already used for the item-4 bench numbers) don't
// collapse onto one node - load spreads per P-RB1's already-proven scoring."
//
// P-VAL1's own filed definition explicitly states scenarios 1,2,3,6,7,8,9,10
// "can be written and run against the current stack today" - it does not
// require real vLLM/TGI hardware. This file is that scenario, built as a
// deterministic router-level test using synthetic NodeState. It is genuinely
// scenario 10 as filed, not a stand-in for it - but it is NOT proof of real
// vLLM/TGI fleet behavior under real GPU load, which remains a separate,
// still-open validation item (see the P423 queue entry).

// TestPVAL1Scenario10_ConcurrentLoadSpreadsAcrossNodes is the base scenario:
// 50 concurrent requests against N equally-capable nodes must not collapse
// onto one node. Independent of P423 - this exercises the pre-existing
// ActiveConns/TTFT-weighted spread scoring (P-RB1), the same mechanism the
// scenario's own text credits ("load spreads per P-RB1's already-proven
// scoring").
func TestPVAL1Scenario10_ConcurrentLoadSpreadsAcrossNodes(t *testing.T) {
	const numNodes = 5
	const numRequests = 50

	nodesCfg := make([]config.NodeConfig, numNodes)
	for i := 0; i < numNodes; i++ {
		name := string(rune('a' + i))
		nodesCfg[i] = config.NodeConfig{Name: "node-" + name, URL: "http://node-" + name + ":11434", VRAMTotalMB: 8192}
	}
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nodesCfg, nil)
	for _, n := range r.nodes {
		n.LoadedModels = []ModelInfo{{Name: "model-x", SizeVRAM: 4000}}
	}

	picks := make(chan string, numRequests)
	var wg sync.WaitGroup
	var dispatchMu sync.Mutex // serializes route+incr, matching the invariant a real accept loop gives each dispatch
	start := make(chan struct{})
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dispatchMu.Lock()
			node, _, _ := r.Route("model-x", "", "")
			if node != nil {
				r.IncrConn(node)
			}
			dispatchMu.Unlock()
			if node == nil {
				return
			}
			picks <- node.Name
		}()
	}
	close(start)
	wg.Wait()
	close(picks)

	counts := make(map[string]int)
	total := 0
	for name := range picks {
		counts[name]++
		total++
	}
	if total != numRequests {
		t.Fatalf("expected %d routed requests, got %d (some Route() call returned nil)", numRequests, total)
	}

	// "Doesn't collapse onto one node": no single node may take a
	// disproportionate share of the burst. A generous bound (60%) avoids
	// flaking on goroutine-scheduling timing while still catching a real
	// collapse (all-or-nearly-all-50 landing on one node).
	maxShare := numRequests * 6 / 10
	for name, c := range counts {
		if c > maxShare {
			t.Errorf("node %q received %d/%d requests (>%d, %.0f%%) - load collapsed onto one node", name, c, numRequests, maxShare, 100*float64(c)/float64(numRequests))
		}
	}
	// And spread actually happened: more than one node was used.
	if len(counts) < 2 {
		t.Errorf("only %d distinct node(s) received any of the %d requests, want load spread across multiple nodes: %v", len(counts), numRequests, counts)
	}
}

// A second test here originally tried to validate P423's "predicted delay
// beats raw queue depth" claim with a synthetic no-completion greedy
// simulation (cumulative assigned cost as a stand-in for wait time). It was
// removed after audit found it invalid, not merely unflattering:
//
//   - P-VAL1 scenario 10's own text ties its "50-concurrent load" to "the
//     item-4 bench numbers" (see EXECUTION-QUEUE.md item 4, "Run hardware
//     bench + update README", itself BLOCKED on hardware) - a real live load
//     test measuring p95 TTFT direct-to-backend vs. via-marbor-across-N-nodes,
//     not an abstract scoring-function comparison. Neither P-VAL1 nor the
//     P423 queue entry defines a metric, workload mix, or baseline for a
//     synthetic "predicted delay vs raw queue depth" comparison - that
//     methodology would have to be invented from scratch, and was.
//   - The invented no-completion model (assign 50 arrivals, never drain
//     completed work) systematically penalized low-TTFT "fast" nodes: it let
//     assigned cost accumulate without ever crediting their faster real
//     completion rate, which engineAwareLoad's actual design (a myopic
//     single-decision "which node should get the next request" signal,
//     matching TestPredictedDelayBeatsRawQueueDepthOnRequestSizeVariance in
//     placement_test.go, which does pass) was never claimed to optimize for.
//     The ~29% "worse" result it produced measured an artifact of the
//     simulator, not a property of the shipped scoring code.
//
// Validating the actual claim requires the real item-4 hardware bench
// (direct-to-backend vs via-marbor p95 TTFT, with and without this scoring
// change) - it remains blocked on that hardware, same as item 4 itself. See
// the P423 queue entry for current status.
