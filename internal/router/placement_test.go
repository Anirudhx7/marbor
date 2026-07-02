package router

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
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
		node, warm := r.Route("model-x", "", "")
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

		node, warm := r.Route("model-x", "", "")
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
		node, _ = r.Route("model-x", "", "")
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
		node, _ := r.Route("model-x", "", "")
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
		node, warm := r.Route("model-x", "", "")
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
		node, warm := r.Route("model-x", "", "")
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

		node, _ := r.Route("model-x", "", "")
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
		node, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" (node-a is in cooldown)", node.Name)
		}

		// Clear cooldown by changing LastErrorAt to >60s ago
		r.nodes[0].mu.Lock()
		r.nodes[0].LastErrorAt = time.Now().Add(-70 * time.Second)
		r.nodes[0].SuccessHistory = nil // reset success history so success rate doesn't skew score
		r.nodes[0].mu.Unlock()

		// Node A is out of cooldown, should win via tiebreaker 'a' < 'b'
		node, _ = r.Route("model-x", "", "")
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
		node, _ := r.Route("model-x", "", "")
		if node.Name != "node-b" {
			t.Errorf("selected node %q, want \"node-b\" due to higher success rate", node.Name)
		}
	})
}
