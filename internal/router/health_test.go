package router

import (
	"context"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	runtimepkg "github.com/ollama-mesh/ollama-mesh/internal/runtime"
)

// panicProbe simulates any bug in a RuntimeProbe implementation - the
// specific cause doesn't matter, only that pollNode must survive it.
type panicProbe struct{}

func (panicProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	panic("simulated probe panic")
}

// TestPollNode_RecoversFromProbePanic guards the reliability boundary added
// after a nil-probe bug crashed the whole mesh process on boot (a single
// persisted node record took down the entire single-process mesh, locking
// the operator out until they wiped mesh.db). No single node's poll -
// whatever the cause - may ever be allowed to panic the process again;
// pollNode must recover, mark that node failed, and let the poll loop
// retry it next cycle instead.
func TestPollNode_RecoversFromProbePanic(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "ollama"},
	}, nil)

	r.nodes[0].mu.Lock()
	r.nodes[0].probe = panicProbe{}
	r.nodes[0].mu.Unlock()

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("pollNode must recover internally, but panic escaped: %v", rec)
			}
		}()
		r.pollNode(r.nodes[0])
	}()

	r.nodes[0].mu.RLock()
	failures := r.nodes[0].Failures
	r.nodes[0].mu.RUnlock()
	if failures != 1 {
		t.Errorf("Failures = %d, want 1 (markFailure should run after recovering the panic)", failures)
	}
}
