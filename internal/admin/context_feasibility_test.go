package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestKVCacheBytesPerToken verifies the real per-token KV-cache formula:
// 2 (K+V) x layers x kv_heads x head_dim x 2 bytes (fp16). Numbers below
// match a Llama-3.1-8B-shaped config (32 layers, 8 KV heads, 4096 hidden
// size, 32 attention heads -> head_dim 128) so the expected byte count can
// be hand-verified, not just re-derived from the same code under test.
func TestKVCacheBytesPerToken(t *testing.T) {
	got := kvCacheBytesPerToken(32, 8, 4096, 32)
	// 2 * 32 * 8 * (4096/32) * 2 = 2*32*8*128*2 = 131072 bytes/token
	want := int64(131072)
	if got != want {
		t.Errorf("kvCacheBytesPerToken() = %d, want %d", got, want)
	}
}

func TestKVCacheBytesPerToken_ZeroAttnHeads(t *testing.T) {
	if got := kvCacheBytesPerToken(32, 8, 4096, 0); got != 0 {
		t.Errorf("expected 0 for zero attention heads (avoid divide-by-zero), got %d", got)
	}
}

// TestComputeContextFeasibility_EstimatedFallback verifies that with arch
// nil, the total byte count matches the pre-existing linear formula exactly
// (unchanged by this feature) and Confidence is "estimated" - never "derived".
func TestComputeContextFeasibility_EstimatedFallback(t *testing.T) {
	sizeMB := int64(4000)
	ctxLen := int64(8192)
	totalEstBytes, fit, cf := computeContextFeasibility(sizeMB, ctxLen, 1.10, 0.15, 8192*1024*1024, "nvidia", nil, "")

	wantMB := int64(float64(sizeMB)*1.10+float64(ctxLen)*0.15) * 1024 * 1024
	if totalEstBytes != wantMB {
		t.Errorf("totalEstBytes = %d, want %d (must match the pre-existing linear formula exactly)", totalEstBytes, wantMB)
	}
	if cf.Confidence != "estimated" {
		t.Errorf("Confidence = %q, want \"estimated\"", cf.Confidence)
	}
	if cf.DeclaredMaxContext != nil {
		t.Error("DeclaredMaxContext must be nil (Unknown) on the Estimated path - never guessed")
	}
	if cf.RecommendedCtx != nil {
		t.Error("RecommendedCtx must never be set on the Estimated path - no recommendation from a rough estimate")
	}
	if fit == "" {
		t.Error("fit must be classified even on the Estimated path")
	}
}

// TestComputeContextFeasibility_Derived verifies the Derived path: with real
// architecture facts, Confidence=="derived", a real declared_max_context is
// populated, and the total differs from what the Estimated formula would
// have produced at the same inputs (proving the real formula actually ran).
func TestComputeContextFeasibility_Derived(t *testing.T) {
	sizeMB := int64(4000)
	ctxLen := int64(32768)
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 131072}

	totalEstBytes, fit, cf := computeContextFeasibility(sizeMB, ctxLen, 1.10, 0.15, 8192*1024*1024, "nvidia", arch, "")

	if cf.Confidence != "derived" {
		t.Fatalf("Confidence = %q, want \"derived\"", cf.Confidence)
	}
	if cf.DeclaredMaxContext == nil || *cf.DeclaredMaxContext != 131072 {
		t.Errorf("DeclaredMaxContext = %v, want 131072", cf.DeclaredMaxContext)
	}
	if cf.ExceedsDeclaredMax {
		t.Error("ExceedsDeclaredMax should be false: 32768 <= 131072")
	}

	estimatedFormulaBytes := int64(float64(sizeMB)*1.10+float64(ctxLen)*0.15) * 1024 * 1024
	if totalEstBytes == estimatedFormulaBytes {
		t.Error("Derived total must differ from the linear Estimated formula's result at these inputs - otherwise the real formula isn't actually being used")
	}
	if cf.KVCacheEstMB <= 0 {
		t.Error("KVCacheEstMB should be > 0 on the Derived path")
	}
	if cf.LimitingFactor == "" {
		t.Error("LimitingFactor should be populated on the Derived path")
	}
	if fit == "" {
		t.Error("fit must be classified on the Derived path")
	}
}

// TestComputeContextFeasibility_ExceedsDeclaredMax verifies the distinct
// "requested beyond model's own trained max" signal, separate from any
// VRAM-fit warning.
func TestComputeContextFeasibility_ExceedsDeclaredMax(t *testing.T) {
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 8192}
	_, _, cf := computeContextFeasibility(4000, 32768, 1.10, 0.15, 8192*1024*1024, "nvidia", arch, "")

	if !cf.ExceedsDeclaredMax {
		t.Error("expected ExceedsDeclaredMax=true: requested 32768 > declared max 8192")
	}
}

// TestComputeContextFeasibility_LimitingFactor verifies the formula
// correctly attributes which term dominates: a tiny model at a huge context
// length should be KV-cache-limited, not weights-limited.
func TestComputeContextFeasibility_LimitingFactor(t *testing.T) {
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 200000}
	// Tiny 100MB model, huge 128K context -> KV cache should dominate.
	_, _, cf := computeContextFeasibility(100, 131072, 1.10, 0.15, 999999*1024*1024, "nvidia", arch, "")
	if cf.LimitingFactor != "kv_cache" {
		t.Errorf("LimitingFactor = %q, want \"kv_cache\" (tiny model, huge context)", cf.LimitingFactor)
	}
}

// TestComputeContextFeasibility_RecommendedCtx verifies the binary-search
// recommendation: when the requested context doesn't fit, a lower context
// that does fit (per the same real formula) is suggested, and it's never
// higher than what was requested nor above the model's declared max.
func TestComputeContextFeasibility_RecommendedCtx(t *testing.T) {
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 131072}
	// A small VRAM budget that the requested 131072 context will not fit,
	// but a much smaller context length would.
	vramCapacityBytes := int64(6) * 1024 * 1024 * 1024 // 6 GB
	_, fit, cf := computeContextFeasibility(4000, 131072, 1.10, 0.15, vramCapacityBytes, "nvidia", arch, "")

	if fit == "green" {
		t.Fatal("test setup invalid: expected requested context not to fit green at this VRAM budget")
	}
	if cf.RecommendedCtx == nil {
		t.Fatal("expected a RecommendedCtx to be suggested when the requested length doesn't fit")
	}
	if *cf.RecommendedCtx >= 131072 {
		t.Errorf("RecommendedCtx = %d, must be less than the requested 131072", *cf.RecommendedCtx)
	}
	if *cf.RecommendedCtx <= 0 {
		t.Errorf("RecommendedCtx = %d, must be positive", *cf.RecommendedCtx)
	}
}

// TestComputeContextFeasibility_NoRecommendationWhenGreen verifies no
// recommendation is offered when the requested context already fits.
func TestComputeContextFeasibility_NoRecommendationWhenGreen(t *testing.T) {
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 131072}
	_, fit, cf := computeContextFeasibility(4000, 4096, 1.10, 0.15, 999999*1024*1024, "nvidia", arch, "")
	if fit != "green" {
		t.Fatalf("test setup invalid: expected green fit, got %q", fit)
	}
	if cf.RecommendedCtx != nil {
		t.Error("RecommendedCtx must be nil when the requested context already fits green")
	}
}

// TestComputeContextFeasibility_UnknownVRAMNoRecommendation verifies that
// when VRAM source is unknown/inferred, no recommendation is fabricated even
// on the Derived path, since classifyFit itself can't classify against an
// unknown capacity.
func TestComputeContextFeasibility_UnknownVRAMNoRecommendation(t *testing.T) {
	arch := &hfArchFacts{NumLayers: 32, NumKVHeads: 8, NumAttnHeads: 32, HiddenSize: 4096, MaxContext: 131072}
	_, fit, cf := computeContextFeasibility(4000, 131072, 1.10, 0.15, 0, "unknown", arch, "")
	if fit != "unknown" {
		t.Errorf("fit = %q, want \"unknown\"", fit)
	}
	if cf.RecommendedCtx != nil {
		t.Error("RecommendedCtx must never be set when vram_source is unknown")
	}
}

// TestFetchHFConfigJSON_Success verifies a well-formed config.json decodes
// into real architecture facts.
func TestFetchHFConfigJSON_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"num_hidden_layers":       32,
			"num_attention_heads":     32,
			"num_key_value_heads":     8,
			"hidden_size":             4096,
			"max_position_embeddings": 131072,
		})
	}))
	defer srv.Close()

	cfg, ok := fetchHFConfigJSONAt(t, srv.URL)
	if !ok {
		t.Fatal("expected ok=true for a complete config.json")
	}
	if cfg.NumHiddenLayers != 32 || cfg.NumAttentionHeads != 32 || cfg.NumKeyValueHeads != 8 || cfg.HiddenSize != 4096 || cfg.MaxPositionEmbeddings != 131072 {
		t.Errorf("unexpected decoded config: %+v", cfg)
	}
}

// TestFetchHFConfigJSON_MHAFallback verifies that a config.json omitting
// num_key_value_heads (dense/MHA architectures) falls back to
// num_attention_heads, per config.json's own documented convention - not a
// guess this codebase invents.
func TestFetchHFConfigJSON_MHAFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"num_hidden_layers":       24,
			"num_attention_heads":     16,
			"hidden_size":             2048,
			"max_position_embeddings": 4096,
		})
	}))
	defer srv.Close()

	cfg, ok := fetchHFConfigJSONAt(t, srv.URL)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if cfg.NumKeyValueHeads != 16 {
		t.Errorf("NumKeyValueHeads = %d, want 16 (fallback to NumAttentionHeads for MHA)", cfg.NumKeyValueHeads)
	}
}

// TestFetchHFConfigJSON_MissingFields verifies an incomplete config.json
// (e.g. missing max_position_embeddings) returns ok=false rather than a
// partially-filled struct.
func TestFetchHFConfigJSON_MissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"num_hidden_layers":   32,
			"num_attention_heads": 32,
			"hidden_size":         4096,
			// max_position_embeddings missing
		})
	}))
	defer srv.Close()

	_, ok := fetchHFConfigJSONAt(t, srv.URL)
	if ok {
		t.Error("expected ok=false when a required field is missing")
	}
}

// TestFetchHFConfigJSON_HeadDimTruncationGuard verifies a config.json where
// hidden_size < num_attention_heads (which would make head_dim truncate to 0
// via integer division, silently zeroing the entire KV-cache term while
// still labeled "derived") is rejected as ok=false rather than accepted with
// a fabricated zero-cost answer.
func TestFetchHFConfigJSON_HeadDimTruncationGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"num_hidden_layers":       32,
			"num_attention_heads":     128,
			"hidden_size":             64, // < num_attention_heads -> head_dim would truncate to 0
			"max_position_embeddings": 8192,
		})
	}))
	defer srv.Close()

	_, ok := fetchHFConfigJSONAt(t, srv.URL)
	if ok {
		t.Error("expected ok=false when hidden_size < num_attention_heads (head_dim would truncate to 0)")
	}
}

// TestFetchHFConfigJSON_NotFound verifies a 404 (most pure-GGUF repos have
// no config.json at all) fails closed to ok=false.
func TestFetchHFConfigJSON_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, ok := fetchHFConfigJSONAt(t, srv.URL)
	if ok {
		t.Error("expected ok=false for a 404 (no config.json)")
	}
}

// fetchHFConfigJSONAt is a test-only helper that calls the uncached
// fetchHFConfigJSONUncached against a given base URL instead of the real
// huggingface.co host, by temporarily swapping hfHTTPClient's Transport to
// redirect requests there - the same override pattern catalog_test.go's
// stubRoundTripper already uses for HF calls, applied here so a single
// httptest.Server can stand in for
// "https://huggingface.co/{repo}/raw/main/config.json" without a real
// network call. Calls the uncached function directly (not the cached
// fetchHFConfigJSON wrapper) since every test in this file reuses the same
// literal repoID "any/repo" against a different mock server - going through
// the 30s TTL cache would make later tests in the same run see an earlier
// test's cached result instead of hitting their own mock.
func fetchHFConfigJSONAt(t *testing.T, baseURL string) (hfConfigJSON, bool) {
	t.Helper()
	orig := hfHTTPClient.Transport
	hfHTTPClient.Transport = redirectRoundTripper{target: baseURL}
	defer func() { hfHTTPClient.Transport = orig }()
	return fetchHFConfigJSONUncached(context.Background(), "any/repo", "")
}

type redirectRoundTripper struct {
	target string
}

func (rt redirectRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := req.Clone(req.Context())
	targetURL, err := url.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	newReq.URL.Scheme = targetURL.Scheme
	newReq.URL.Host = targetURL.Host
	return http.DefaultTransport.RoundTrip(newReq)
}

// TestHandleModelRepo_ContextFeasibility_Derived is a handler-level test
// (acceptance criterion 1/2) confirming that for a model already downloaded
// to an Ollama node, handleModelRepo's context_feasibility comes back
// Confidence=="derived" with a real declared_max_context sourced from
// /api/show - not the hardcoded linear estimate.
func TestHandleModelRepo_ContextFeasibility_Derived(t *testing.T) {
	fakeHF := `{
		"id": "testorg/testmodel-gguf",
		"downloads": 1000,
		"likes": 10,
		"tags": ["text-generation"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "testmodel-Q4_K_M.gguf", "size": 4000000000}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	downloadedTag := "hf.co/testorg/testmodel-gguf:Q4_K_M"
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{{"name": downloadedTag, "size": 4000000000}},
			})
		case r.URL.Path == "/api/show" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"model_info": map[string]interface{}{
					"general.architecture":          "llama",
					"llama.context_length":          131072,
					"llama.block_count":             32,
					"llama.attention.head_count":    32,
					"llama.attention.head_count_kv": 8,
					"llama.embedding_length":        4096,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=testorg/testmodel-gguf&ctx=32768", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(resp.Variants))
	}
	cf := resp.Variants[0].ContextFeasibility
	if cf.Confidence != "derived" {
		t.Errorf("Confidence = %q, want \"derived\" (a downloaded model's /api/show should have been used)", cf.Confidence)
	}
	if cf.DeclaredMaxContext == nil || *cf.DeclaredMaxContext != 131072 {
		t.Errorf("DeclaredMaxContext = %v, want 131072", cf.DeclaredMaxContext)
	}
	if !resp.Variants[0].Downloaded {
		t.Error("expected variant to be marked downloaded")
	}
}

// TestHandleModelRepo_ContextFeasibility_EstimatedFallback is a
// handler-level test (acceptance criterion 6) confirming that a pure-GGUF
// repo with no local Ollama copy (so /api/show can never be called) falls
// back to Confidence=="estimated" with no declared_max_context and no
// recommendation - never presented as more certain than it is.
func TestHandleModelRepo_ContextFeasibility_EstimatedFallback(t *testing.T) {
	fakeHF := `{
		"id": "someorg/somemodel-gguf",
		"downloads": 500,
		"likes": 5,
		"tags": ["text-generation"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "somemodel-Q4_K_M.gguf", "size": 4000000000}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	// mockOllamaServer only serves /api/tags (empty of this model) and has no
	// /api/show handler at all - simulating a model never downloaded here.
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=someorg/somemodel-gguf&ctx=8192", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(resp.Variants))
	}
	cf := resp.Variants[0].ContextFeasibility
	if cf.Confidence != "estimated" {
		t.Errorf("Confidence = %q, want \"estimated\"", cf.Confidence)
	}
	if cf.DeclaredMaxContext != nil {
		t.Error("DeclaredMaxContext must be nil/omitted on the Estimated path")
	}
	if cf.RecommendedCtx != nil {
		t.Error("RecommendedCtx must never be set on the Estimated path")
	}
}

// TestHandleModelRepo_CtxOverflowGuard is a regression test: an absurdly
// large ?ctx= value must be rejected (falling back to the 8192 default)
// rather than accepted verbatim, since perTokenBytes*requestedCtx in
// computeContextFeasibility's Derived path can otherwise overflow int64 and
// wrap negative - which classifyFit would then read as comfortably "green",
// a fabricated-fit result rather than an honest rejection of a bogus input.
func TestHandleModelRepo_CtxOverflowGuard(t *testing.T) {
	fakeHF := `{
		"id": "someorg/somemodel-gguf",
		"downloads": 500,
		"likes": 5,
		"tags": ["text-generation"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "somemodel-Q4_K_M.gguf", "size": 4000000000}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=someorg/somemodel-gguf&ctx=9223372036854775807", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(resp.Variants))
	}
	if resp.Variants[0].ContextFeasibility.RequestedCtx != 8192 {
		t.Errorf("RequestedCtx = %d, want the default 8192 (an out-of-range ctx must be rejected, not clamped/wrapped)", resp.Variants[0].ContextFeasibility.RequestedCtx)
	}
	if resp.Variants[0].VRAMEstMB <= 0 {
		t.Errorf("VRAMEstMB = %d, want a positive estimate - a negative/overflowed value would classify as a fabricated \"green\" fit", resp.Variants[0].VRAMEstMB)
	}
}
