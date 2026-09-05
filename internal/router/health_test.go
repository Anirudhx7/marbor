package router

import (
	"context"
	"fmt"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

// panicProbe simulates any bug in a RuntimeProbe implementation - the
// specific cause doesn't matter, only that pollNode must survive it.
type panicProbe struct{}

func (panicProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	panic("simulated probe panic")
}

// TestPollNode_RecoversFromProbePanic guards the reliability boundary added
// after a nil-probe bug crashed the whole marbor process on boot (a single
// persisted node record took down the entire single-process marbor, locking
// the operator out until they wiped marbor.db). No single node's poll -
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

// fixedErrProbe always fails Probe with a fixed error - used to simulate a
// specific failure shape (e.g. llamacpp's checkHealth wrapping "/health
// returned 404") without standing up a real httptest.Server per case.
type fixedErrProbe struct{ err error }

func (p fixedErrProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	return runtimepkg.ProbeResult{}, p.err
}

// TestPollNode_SetsRuntimeMismatchHint_LlamaCppHealth404 covers the
// regression where a node currently probed as llamacpp whose /health check fails
// specifically with a 404 (the exact real-world signature an MLX node
// auto-detected as llamacpp produces, since mlx_lm.server has no /health
// route at all) must surface RuntimeMismatchHint instead of just sitting
// silently unhealthy with no explanation.
func TestPollNode_SetsRuntimeMismatchHint_LlamaCppHealth404(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:8080", Runtime: "llamacpp"},
	}, nil)

	n := r.nodes[0]
	n.mu.Lock()
	n.probe = fixedErrProbe{err: fmt.Errorf("llamacpp probe: /health returned 404")}
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	hint := n.RuntimeMismatchHint
	n.mu.RUnlock()
	if hint == "" {
		t.Error("RuntimeMismatchHint not set for a llamacpp node whose /health returned 404")
	}
}

// TestPollNode_NoRuntimeMismatchHint_GenericFailure guards the false-positive
// direction: a llamacpp node failing for an ordinary reason (connection
// refused, timeout, 500, etc.) must NOT get the MLX hint - it's a real,
// observed-fact hint, not a generic "this node is unhealthy" catch-all.
func TestPollNode_NoRuntimeMismatchHint_GenericFailure(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:8080", Runtime: "llamacpp"},
	}, nil)

	n := r.nodes[0]
	n.mu.Lock()
	n.probe = fixedErrProbe{err: fmt.Errorf("llamacpp probe: health request: connection refused")}
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	hint := n.RuntimeMismatchHint
	n.mu.RUnlock()
	if hint != "" {
		t.Errorf("RuntimeMismatchHint = %q, want empty for a generic connection failure (not a /health 404)", hint)
	}
}

// TestPollNode_ClearsRuntimeMismatchHint_OnSuccess guards the recovery path:
// once a node starts probing successfully again (e.g. an operator fixed a
// misconfigured reverse proxy that was 404ing /health), a stale hint from an
// earlier failed poll must not linger and mislead.
func TestPollNode_ClearsRuntimeMismatchHint_OnSuccess(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:8080", Runtime: "llamacpp"},
	}, nil)

	n := r.nodes[0]
	n.mu.Lock()
	n.RuntimeMismatchHint = "stale hint from an earlier failed poll"
	n.probe = nonOllamaZeroVRAMProbe{models: []runtimepkg.LoadedModel{{Name: "some-model", SizeVRAMBytes: 0}}}
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	hint := n.RuntimeMismatchHint
	n.mu.RUnlock()
	if hint != "" {
		t.Errorf("RuntimeMismatchHint = %q, want empty after a successful probe", hint)
	}
}

// TestAttributeSoleModelVRAM_SingleModel_UnknownSize fills in the sole
// loaded model's VRAM size from the node's real VRAMUsedMB: a
// non-Ollama runtime that reports SizeVRAM=0 for its one loaded model
// should get that model attributed the node's real observed usage once one
// exists, rather than staying at a fabricated-looking 0 forever.
func TestAttributeSoleModelVRAM_SingleModel_UnknownSize(t *testing.T) {
	n := &NodeState{
		LoadedModels: []ModelInfo{{Name: "llama-3-70b", SizeVRAM: 0}},
		VRAMUsedMB:   40000,
	}
	attributeSoleModelVRAM(n)
	want := int64(40000) * 1024 * 1024
	if n.LoadedModels[0].SizeVRAM != want {
		t.Errorf("SizeVRAM = %d, want %d (attributed from node VRAMUsedMB)", n.LoadedModels[0].SizeVRAM, want)
	}
}

// TestAttributeSoleModelVRAM_NeverOverridesRealSize: Ollama already reports
// a real per-model size_vram - attribution must never override it, even if
// it happens to differ from the node's rolled-up VRAMUsedMB (e.g. mid-poll
// transition).
func TestAttributeSoleModelVRAM_NeverOverridesRealSize(t *testing.T) {
	n := &NodeState{
		LoadedModels: []ModelInfo{{Name: "llama3:8b", SizeVRAM: 8192 * 1024 * 1024}},
		VRAMUsedMB:   40000,
	}
	attributeSoleModelVRAM(n)
	want := int64(8192) * 1024 * 1024
	if n.LoadedModels[0].SizeVRAM != want {
		t.Errorf("SizeVRAM = %d, want %d (must not override an already-known real size)", n.LoadedModels[0].SizeVRAM, want)
	}
}

// TestAttributeSoleModelVRAM_MultipleModels_StaysUnknown: with more than one
// model loaded on the node, per-process VRAM correlation is not observable
// (no PID-to-model correlation over these HTTP probes) - attribution must
// leave every unknown size at 0 rather than guess a split.
func TestAttributeSoleModelVRAM_MultipleModels_StaysUnknown(t *testing.T) {
	n := &NodeState{
		LoadedModels: []ModelInfo{
			{Name: "model-a", SizeVRAM: 0},
			{Name: "model-b", SizeVRAM: 0},
		},
		VRAMUsedMB: 40000,
	}
	attributeSoleModelVRAM(n)
	for i, m := range n.LoadedModels {
		if m.SizeVRAM != 0 {
			t.Errorf("LoadedModels[%d].SizeVRAM = %d, want 0 (must not guess a split across multiple models)", i, m.SizeVRAM)
		}
	}
}

// TestAttributeSoleModelVRAM_NoVRAMReading_StaysUnknown: no known VRAMUsedMB
// (e.g. no agent, non-Ollama) - must stay 0, exactly today's behavior, not a
// fabricated guess.
func TestAttributeSoleModelVRAM_NoVRAMReading_StaysUnknown(t *testing.T) {
	n := &NodeState{
		LoadedModels: []ModelInfo{{Name: "llama-3-70b", SizeVRAM: 0}},
		VRAMUsedMB:   0,
	}
	attributeSoleModelVRAM(n)
	if n.LoadedModels[0].SizeVRAM != 0 {
		t.Errorf("SizeVRAM = %d, want 0 (no real VRAM reading available to attribute)", n.LoadedModels[0].SizeVRAM)
	}
}

// nonOllamaZeroVRAMProbe simulates a non-Ollama runtime: reports a
// loaded model but always SizeVRAMBytes=0, exactly like
// internal/runtime/{vllm,tgi,llamacpp,mlx}.go today.
type nonOllamaZeroVRAMProbe struct {
	models []runtimepkg.LoadedModel
}

func (p nonOllamaZeroVRAMProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	return runtimepkg.ProbeResult{LoadedModels: p.models, VRAMUsedMB: 0}, nil
}

// TestPollNode_PreservesAgentSourcedVRAM_NonOllama covers the regression
// for the race pollNode and pollAgentHosts run concurrently on the same
// poll tick (Router.Start): a real, previously agent-reported VRAMUsedMB
// for a non-Ollama node must not be stomped back to 0 by this poll cycle's
// own always-zero runtime-API reading. Before this fix, pollNode's default
// branch unconditionally set n.VRAMUsedMB = psUsedMB (always 0 for
// non-Ollama), which would flicker a real agent reading back to unknown on
// roughly every other tick depending on goroutine completion order.
func TestPollNode_PreservesAgentSourcedVRAM_NonOllama(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:8000", Runtime: "vllm"},
	}, nil)

	n := r.nodes[0]
	n.mu.Lock()
	n.probe = nonOllamaZeroVRAMProbe{models: []runtimepkg.LoadedModel{{Name: "llama-3-70b", SizeVRAMBytes: 0}}}
	// Simulate an already-applied agent telemetry reading from a concurrent
	// pollAgentHosts cycle (applyAgentTelemetry sets exactly this pair).
	n.VRAMUsedMB = 40000
	n.VRAMSource = "agent"
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMUsedMB != 40000 {
		t.Errorf("VRAMUsedMB = %d, want 40000 (a zero non-Ollama psUsedMB reading must not stomp an agent-sourced value)", n.VRAMUsedMB)
	}
	if n.VRAMSource != "agent" {
		t.Errorf("VRAMSource = %q, want \"agent\" (preserved, not downgraded to declared/none)", n.VRAMSource)
	}
	if len(n.LoadedModels) != 1 || n.LoadedModels[0].SizeVRAM != 40000*1024*1024 {
		t.Errorf("LoadedModels[0].SizeVRAM not attributed from the preserved agent VRAMUsedMB: %+v", n.LoadedModels)
	}
}

// TestPollNode_NoAgent_NonOllama_StaysZero is the explicit "old marbor
// talking to an old/no agent" regression: a non-Ollama node with no agent
// telemetry ever applied must behave exactly as it does today - VRAM=0,
// no crash, no fabricated data - not a side effect of the non-Ollama VRAM-attribution changes.
func TestPollNode_NoAgent_NonOllama_StaysZero(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:8000", Runtime: "vllm"},
	}, nil)

	n := r.nodes[0]
	n.mu.Lock()
	n.probe = nonOllamaZeroVRAMProbe{models: []runtimepkg.LoadedModel{{Name: "llama-3-70b", SizeVRAMBytes: 0}}}
	n.mu.Unlock()

	r.pollNode(n)

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMUsedMB != 0 {
		t.Errorf("VRAMUsedMB = %d, want 0 (no agent, non-Ollama - must stay unknown, not guessed)", n.VRAMUsedMB)
	}
	if n.VRAMSource != "none" {
		t.Errorf("VRAMSource = %q, want \"none\"", n.VRAMSource)
	}
	if len(n.LoadedModels) != 1 || n.LoadedModels[0].SizeVRAM != 0 {
		t.Errorf("LoadedModels[0].SizeVRAM = %+v, want 0 (nothing to attribute from)", n.LoadedModels)
	}
}

// residentProbe simulates a runtime that reports exactly the given models as
// resident, for testing poll-driven reservation cleanup.
type residentProbe struct {
	models []runtimepkg.LoadedModel
}

func (p residentProbe) Probe(ctx context.Context, nodeURL string) (runtimepkg.ProbeResult, error) {
	return runtimepkg.ProbeResult{LoadedModels: p.models}, nil
}

// TestPollNode_ClearsWarmReservationOnConfirmedResidency is the
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
