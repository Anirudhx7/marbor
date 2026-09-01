package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestKVCacheBytesPerToken_GQAvsMHA verifies the formula produces a smaller
// per-token footprint for a GQA model (fewer KV heads than attention heads)
// than an otherwise-identical MHA model (P405 acceptance criterion: same
// disk size, different KV-cache footprint).
func TestKVCacheBytesPerToken_GQAvsMHA(t *testing.T) {
	// Same layers/hiddenSize/attnHeads; GQA groups 4 attention heads per KV head.
	mha := KVCacheBytesPerToken(32, 32, 4096, 32) // numKVHeads == numAttnHeads
	gqa := KVCacheBytesPerToken(32, 8, 4096, 32)  // numKVHeads < numAttnHeads
	if gqa >= mha {
		t.Fatalf("GQA per-token bytes (%d) should be smaller than MHA (%d)", gqa, mha)
	}
	if gqa != mha/4 {
		t.Errorf("GQA (1 KV head per 4 attn heads) should be exactly 1/4 of MHA: got gqa=%d mha=%d", gqa, mha)
	}
}

func TestKVCacheBytesPerToken_ZeroAttnHeadsGuarded(t *testing.T) {
	if got := KVCacheBytesPerToken(32, 8, 4096, 0); got != 0 {
		t.Errorf("numAttnHeads<=0 should return 0, got %d", got)
	}
}

// TestEstimateContextAwareBytes_ContextLengthMatters covers the P405
// acceptance criterion that a 1K-token request and a 32K-token request
// against the same model produce different size estimates.
func TestEstimateContextAwareBytes_ContextLengthMatters(t *testing.T) {
	const sizeMB = 8000 // 8GB weights, e.g. an 8B Q4 model
	small := EstimateContextAwareBytes(sizeMB, 1000, "ollama", nil)
	large := EstimateContextAwareBytes(sizeMB, 32000, "ollama", nil)
	if large <= small {
		t.Fatalf("32K-context estimate (%d) should exceed 1K-context estimate (%d)", large, small)
	}
}

// TestEstimateContextAwareBytes_GQAvsMHANoLongerTie covers the P405
// acceptance criterion that a GQA model and an MHA model of the same disk
// size no longer tie in headroom estimates once real architecture facts are
// available.
func TestEstimateContextAwareBytes_GQAvsMHANoLongerTie(t *testing.T) {
	const sizeMB = 8000
	const ctx = 32000
	mha := &ModelArchFacts{NumLayers: 32, NumKVHeads: 32, NumAttnHeads: 32, HiddenSize: 4096}
	gqa := &ModelArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096}

	mhaBytes := EstimateContextAwareBytes(sizeMB, ctx, "ollama", mha)
	gqaBytes := EstimateContextAwareBytes(sizeMB, ctx, "ollama", gqa)
	if mhaBytes == gqaBytes {
		t.Fatalf("same disk size (%d MB) should no longer tie for GQA vs MHA at long context: both estimated %d bytes", sizeMB, mhaBytes)
	}
	if gqaBytes >= mhaBytes {
		t.Errorf("GQA estimate (%d) should be smaller than MHA estimate (%d) at the same context length", gqaBytes, mhaBytes)
	}
}

func TestEstimateContextAwareBytes_NoArchFallsBackToEstimatedPath(t *testing.T) {
	const sizeMB = 8000
	const ctx = 1000
	gguf := EstimateContextAwareBytes(sizeMB, ctx, "ollama", nil)
	want := int64(float64(sizeMB)*GGUFOverheadMult+float64(ctx)*GGUFPerTokenMBFallback) * 1024 * 1024
	if gguf != want {
		t.Errorf("nil-arch estimate should match the estimated-path formula exactly: got %d, want %d", gguf, want)
	}

	safetensors := EstimateContextAwareBytes(sizeMB, ctx, "vllm", nil)
	wantSafetensors := int64(float64(sizeMB)*SafetensorsOverheadMult+float64(ctx)*SafetensorsPerTokenMBFallback) * 1024 * 1024
	if safetensors != wantSafetensors {
		t.Errorf("vLLM (safetensors family) should use the safetensors constants: got %d, want %d", safetensors, wantSafetensors)
	}
}

func TestGGUFOnlyRuntime(t *testing.T) {
	cases := map[string]bool{
		"":         true,
		"ollama":   true,
		"llamacpp": true,
		"vllm":     false,
		"tgi":      false,
		"mlx":      false,
	}
	for runtime, want := range cases {
		if got := GGUFOnlyRuntime(runtime); got != want {
			t.Errorf("GGUFOnlyRuntime(%q) = %v, want %v", runtime, got, want)
		}
	}
}

// TestSetModelArchFacts_RoundTrip verifies the opportunistic-cache push/read
// path admin/catalog.go relies on.
func TestSetModelArchFacts_RoundTrip(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nil, nil)
	if _, ok := r.modelArchFactsFor("never-looked-up"); ok {
		t.Fatal("a model never pushed should have no cached facts")
	}
	facts := ModelArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096}
	r.SetModelArchFacts("llama3.1:8b", facts)
	got, ok := r.modelArchFactsFor("llama3.1:8b")
	if !ok || got != facts {
		t.Errorf("modelArchFactsFor after SetModelArchFacts = (%+v, %v), want (%+v, true)", got, ok, facts)
	}
}

// TestSetContextWindows_RoundTrip verifies the admin-pushed context-window
// cache ensureHeadroom reads.
func TestSetContextWindows_RoundTrip(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nil, nil)
	if _, ok := r.contextWindowFor("llama3.1:8b"); ok {
		t.Fatal("no context window pushed yet should report not-configured")
	}
	r.SetContextWindows(map[string]int{"llama3.1:8b": 32768})
	window, ok := r.contextWindowFor("llama3.1:8b")
	if !ok || window != 32768 {
		t.Errorf("contextWindowFor after SetContextWindows = (%d, %v), want (32768, true)", window, ok)
	}
}
