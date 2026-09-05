package router

// ModelArchFacts holds the transformer architecture facts needed to compute a
// real per-token KV-cache footprint, mirroring internal/admin's hfArchFacts
// (Model Advisor's own architecture-facts struct). Router owns this type
// (rather than importing admin's) because internal/admin already imports
// internal/router - admin importing router is fine, router importing admin
// would be a cycle. admin/catalog.go pushes facts here (SetModelArchFacts)
// whenever it resolves them for a model the operator has looked up in the
// Model Advisor; it never invents them - a model never looked up simply
// has no entry here and falls back to the estimated-path formula below.
type ModelArchFacts struct {
	NumLayers    int64
	NumKVHeads   int64
	NumAttnHeads int64
	HiddenSize   int64
}

// KVCacheBytesPerToken computes the real per-token KV-cache footprint from
// known transformer architecture facts: 2 (one for K, one for V) x layers x
// kv_heads x head_dim x 2 bytes (fp16/bf16, the default KV-cache dtype across
// llama.cpp/Ollama/vLLM/TGI absent an explicit quantized-KV-cache
// configuration, which this codebase has no way to detect). head_dim is
// derived (hiddenSize/numAttnHeads), not a separate metadata field.
//
// This is the single source of truth for the formula - internal/admin's
// Model Advisor (catalog.go's computeContextFeasibility) calls this exported
// version instead of maintaining its own copy, since admin already imports
// router and the reverse import is not possible (see ModelArchFacts).
func KVCacheBytesPerToken(numLayers, numKVHeads, hiddenSize, numAttnHeads int64) int64 {
	if numAttnHeads <= 0 {
		return 0
	}
	headDim := hiddenSize / numAttnHeads
	return 2 * numLayers * numKVHeads * headDim * 2
}

// Estimated-path overhead constants for EstimateContextAwareBytes's two
// runtime families, mirroring internal/admin/catalog.go's
// ggufOverheadMult/ggufPerTokenMBFallback/safetensorsOverheadMult/
// safetensorsPerTokenMBFallback (same values, same rationale: vLLM/TGI/MLX
// carry more runtime overhead - PagedAttention KV cache, CUDA graph buffers -
// than llama.cpp). Exported so catalog.go can reference these instead of
// keeping a second copy of the numbers.
const (
	GGUFOverheadMult              = 1.10
	GGUFPerTokenMBFallback        = 0.15
	SafetensorsOverheadMult       = 1.20
	SafetensorsPerTokenMBFallback = 0.20
)

// FragmentationOverheadMult models a different phenomenon than the two
// constants above: PagedAttention-style block allocators (vLLM, TGI, and
// similar) plus CUDA graph/allocator bookkeeping consume real VRAM beyond the
// sum of already-loaded models' reported bytes, so treating a node's reported
// used bytes as the complete picture understates true consumption. Applied
// at the headroom-decision layer (eviction.go's EvictForHeadroom,
// ModelFitsAnyHealthyNode, ensureHeadroom; placement.go's computeNodeScore
// free_vram_headroom factor) to the "used" side of a free = total - used
// computation, never to the raw health.go VRAMUsedMB measurement itself
// (that stays a real, unmodified measurement). Distinct from
// GGUFOverheadMult/SafetensorsOverheadMult, which instead correct a
// *disk-size-to-VRAM* estimate for one specific model being sized - this
// constant corrects the *already-used* figure for allocator slack regardless
// of which model is being evaluated. 8% is the midpoint of the audited 5-10%
// range for allocator/PagedAttention fragmentation overhead.
const FragmentationOverheadMult = 1.08

// GGUFOnlyRuntime reports whether a node runtime only ever loads GGUF weight
// files (Ollama, llama.cpp) as opposed to standard HF safetensors repos
// (vLLM, TGI, MLX). An empty runtime defaults to the historical GGUF-only
// behavior (Ollama). Mirrors admin/catalog.go's ggufOnlyRuntime, which
// classifies for a different purpose (VRAM capacity basis) and is left
// unexported/unchanged there to avoid coupling that call path to this one.
func GGUFOnlyRuntime(runtime string) bool {
	switch runtime {
	case "", "ollama", "llamacpp":
		return true
	default:
		return false
	}
}

// runtimeOverheadConstants returns the (overheadMult, perTokenMBFallback)
// pair for a node's runtime family, for use by EstimateContextAwareBytes's
// estimated-path fallback.
func runtimeOverheadConstants(runtime string) (overheadMult, perTokenMBFallback float64) {
	if GGUFOnlyRuntime(runtime) {
		return GGUFOverheadMult, GGUFPerTokenMBFallback
	}
	return SafetensorsOverheadMult, SafetensorsPerTokenMBFallback
}

// EstimateContextAwareBytes estimates the total VRAM footprint (weights +
// KV-cache) for loading model of sizeMB at requestedCtx tokens of context on
// a node running runtime. When arch is non-nil (real architecture facts are
// cached for this model, pushed by admin/catalog.go's Model Advisor path),
// it uses the real per-token KV-cache formula above - this is what lets a
// GQA model (few KV heads) and an MHA model (KV heads == attention heads) of
// the same disk size stop tying in headroom estimates, since GQA's KV-cache
// footprint per token is genuinely smaller. When arch is nil, it falls back
// to the same linear estimate admin/catalog.go's computeContextFeasibility
// uses when it has no architecture facts either (sizeMB*overheadMult +
// requestedCtx*perTokenMBFallback) - still context-length-aware (a 1K-token
// and a 32K-token request no longer tie), just not GQA-aware. requestedCtx
// <= 0 means no context-length signal was available at all; callers should
// treat that as "use the pre-existing size-only estimate" rather than call
// this function (never fabricate a context length that wasn't observed
// or configured).
func EstimateContextAwareBytes(sizeMB, requestedCtx int64, runtime string, arch *ModelArchFacts) int64 {
	overheadMult, perTokenMBFallback := runtimeOverheadConstants(runtime)
	if arch == nil {
		return int64(float64(sizeMB)*overheadMult+float64(requestedCtx)*perTokenMBFallback) * 1024 * 1024
	}
	perTokenBytes := KVCacheBytesPerToken(arch.NumLayers, arch.NumKVHeads, arch.HiddenSize, arch.NumAttnHeads)
	kvBytes := perTokenBytes * requestedCtx
	weightBytes := int64(float64(sizeMB) * overheadMult * 1024 * 1024)
	return weightBytes + kvBytes
}

// SetModelArchFacts caches architecture facts for model, pushed opportunistically
// by admin/catalog.go's Model Advisor handlers whenever they successfully
// resolve real transformer architecture facts (GGUF /api/show metadata or an
// HF repo's config.json) for a model the operator looked up. Safe for
// concurrent use. A model never looked up in the Advisor simply has no entry
// here - ensureHeadroom/ModelFitsAnyHealthyNode fall back to the
// non-GQA-aware estimated path for it (see EstimateContextAwareBytes), never
// a fabricated guess.
func (r *Router) SetModelArchFacts(model string, facts ModelArchFacts) {
	r.archFactsMu.Lock()
	defer r.archFactsMu.Unlock()
	if r.modelArchFacts == nil {
		r.modelArchFacts = make(map[string]ModelArchFacts)
	}
	r.modelArchFacts[model] = facts
}

// modelArchFactsFor returns the cached architecture facts for model, if any.
func (r *Router) modelArchFactsFor(model string) (ModelArchFacts, bool) {
	r.archFactsMu.RLock()
	defer r.archFactsMu.RUnlock()
	facts, ok := r.modelArchFacts[model]
	return facts, ok
}

// SetContextWindows replaces the operator-declared per-model context-window
// map (config.Config.ContextWindows), pushed by admin at startup and on every
// settings update so ensureHeadroom (which runs on router-internal proactive
// warm paths with no live per-request context length available) has a
// context-length signal to use. Mirrors the existing SetTimezone/SetLiteLLM
// push pattern. A model absent from this map has no declared window, and
// ensureHeadroom falls back to the pre-existing size-only estimate for it
// (never guess an undeclared context length).
func (r *Router) SetContextWindows(cw map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contextWindows = cw
}

// contextWindowFor returns the operator-declared context window for model,
// and whether one is configured. Guarded by r.mu, same as the other
// SetXConfig-pushed fields it lives alongside.
func (r *Router) contextWindowFor(model string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	window, ok := r.contextWindows[model]
	return window, ok
}
