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

// residentProbe simulates a runtime that reports exactly the given models as
// resident, for testing poll-driven reservation cleanup (P51).
type residentProbe struct {
	models []runtimepkg.LoadedModel
}

func (p residentProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	return runtimepkg.ProbeResult{LoadedModels: p.models}, nil
}

// TestPollNode_ClearsWarmReservationOnConfirmedResidency is the P51
// reservation-cleanup regression test: once a real poll confirms a model is
// actually resident, any hot-path or proactive VRAM reservation for that
// (node, model) must be cleared immediately rather than left to expire after
// the full warmReservationTTL, which would otherwise double-count it against
// the now-real VRAMUsedMB for up to several minutes.
func TestPollNode_ClearsWarmReservationOnConfirmedResidency(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "ollama"},
	}, nil)

	r.nodes[0].mu.Lock()
	r.nodes[0].probe = residentProbe{models: []runtimepkg.LoadedModel{
		{Name: "model-x", SizeVRAMBytes: 3000 * mib},
	}}
	r.nodes[0].mu.Unlock()

	// Simulate a cold-start reservation made moments earlier on the request path.
	r.reserveWarmBytes("gpu-0", "model-x", 3000*mib)
	if got := r.PendingPrewarmBytes("gpu-0"); got != 3000*mib {
		t.Fatalf("PendingPrewarmBytes(gpu-0) = %d before poll, want %d", got, 3000*mib)
	}

	r.pollNode(r.nodes[0])

	if got := r.PendingPrewarmBytes("gpu-0"); got != 0 {
		t.Errorf("PendingPrewarmBytes(gpu-0) = %d after a poll confirming residency, want 0 (reservation should be cleared, not left to expire via TTL)", got)
	}
}
