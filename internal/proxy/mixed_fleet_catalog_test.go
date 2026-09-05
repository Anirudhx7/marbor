package proxy

// mixed_fleet_catalog_test.go -- locks in the cross-runtime model catalog
// contract for a mixed fleet (e.g. an Ollama node serving one model and a
// vLLM node serving another).
//
// GET /v1/models is built from each node's LoadedModels (router.go
// pollNode/health.go), which every runtime probe (Ollama, vLLM, TGI,
// llama.cpp) populates identically - so it already lists models across every
// runtime, not just Ollama's. The one real routing constraint is that
// Ollama-native paths (/api/chat, /api/generate) only reach Ollama-runtime
// nodes (see TestMultiBackend_VLLMOnly_ApiChatReturns503 in
// multibackend_e2e_test.go); /v1/... paths reach any runtime. These tests
// pin both halves of that contract together so a future change to catalog
// aggregation or runtime filtering can't silently regress either one.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// newMixedFleetHandler builds a Handler with one Ollama node serving
// ollamaModel and one vLLM node serving vllmModel, both healthy. hitOllama/
// hitVLLM (if non-nil) are called on every request each mock backend
// receives, so a test can tell which node actually handled a request.
func newMixedFleetHandler(t *testing.T, ollamaModel, vllmModel string, hitOllama, hitVLLM func()) (*Handler, *httptest.Server, *httptest.Server) {
	t.Helper()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hitOllama != nil {
			hitOllama()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi from ollama"}}]}`)) //nolint:errcheck
	}))
	t.Cleanup(ollamaSrv.Close)

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hitVLLM != nil {
			hitVLLM()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi from vllm"}}]}`)) //nolint:errcheck
	}))
	t.Cleanup(vllmSrv.Close)

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "ollama-node", URL: ollamaSrv.URL, Runtime: "ollama"},
		{Name: "vllm-node", URL: vllmSrv.URL, Runtime: "vllm"},
	}, nil)

	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		switch n.Name {
		case "ollama-node":
			n.LoadedModels = []router.ModelInfo{{Name: ollamaModel}}
		case "vllm-node":
			n.LoadedModels = []router.ModelInfo{{Name: vllmModel}}
		}
		n.Unlock()
	}

	a := admin.NewServer(r, nil, config.Config{})
	return NewHandler(r, a, nil), ollamaSrv, vllmSrv
}

// TestMixedFleetV1ModelsListsBothRuntimes verifies GET /v1/models includes
// models served by both an Ollama node and a vLLM node in the same fleet -
// the catalog aggregation a client's /model picker (e.g. OpenClaw's
// openai-completions provider) depends on.
func TestMixedFleetV1ModelsListsBothRuntimes(t *testing.T) {
	h, _, _ := newMixedFleetHandler(t, "qwen2.5", "gemma3", nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}

	seen := make(map[string]bool)
	for _, m := range resp.Data {
		seen[m.ID] = true
	}
	if !seen["qwen2.5"] {
		t.Errorf("/v1/models missing Ollama-runtime model %q: %+v", "qwen2.5", resp.Data)
	}
	if !seen["gemma3"] {
		t.Errorf("/v1/models missing vLLM-runtime model %q: %+v", "gemma3", resp.Data)
	}
}

// TestMixedFleetV1ChatReachesEitherRuntime verifies /v1/chat/completions
// (the OpenAI-compatible path a client should use for a mixed fleet) can
// reach a model served by the vLLM node, not just the Ollama one.
func TestMixedFleetV1ChatReachesEitherRuntime(t *testing.T) {
	h, _, _ := newMixedFleetHandler(t, "qwen2.5", "gemma3", nil, nil)

	body := bytes.NewReader([]byte(`{"model":"gemma3","messages":[{"role":"user","content":"hi"}]}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/chat/completions for vLLM-served model: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMixedFleetAPIChatMisroutesVLLMModelToOllamaNode documents a real, more
// subtle constraint than a clean rejection: when a fleet has AT LEAST ONE
// healthy Ollama node, /api/chat for a model that is only actually served by
// a vLLM node does NOT 503 - the runtime filter only checks "is this node's
// runtime Ollama", not "does this node actually serve the requested model",
// so the request is forwarded to the Ollama node anyway (which would fail or
// behave unexpectedly at the real Ollama layer for an unknown model name).
// TestMultiBackend_VLLMOnly_ApiChatReturns503 (multibackend_e2e_test.go)
// covers the only case that DOES 503: zero Ollama nodes in the fleet at all.
// This is the concrete reason an Ollama-native client (OpenClaw's native
// "ollama" provider, which always calls /api/chat) cannot safely reach
// vLLM/TGI/llama.cpp-hosted models in a mixed fleet - it must use
// /v1/chat/completions instead, which has no such runtime-blind misroute.
func TestMixedFleetAPIChatMisroutesVLLMModelToOllamaNode(t *testing.T) {
	var hitOllama, hitVLLM bool
	h, _, _ := newMixedFleetHandler(t, "qwen2.5", "gemma3",
		func() { hitOllama = true },
		func() { hitVLLM = true },
	)

	body := bytes.NewReader([]byte(`{"model":"gemma3","messages":[{"role":"user","content":"hi"}]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !hitOllama {
		t.Error("expected /api/chat for a vLLM-only model to still be forwarded to the Ollama node (runtime filter is protocol-based, not model-aware) - if this now fails at the marbor level instead, update this test and the client guidance that assumes the misroute")
	}
	if hitVLLM {
		t.Error("request for a vLLM-served model reached the vLLM node via /api/chat - the Ollama-native path should never be able to reach a non-Ollama node")
	}
}

// TestMixedFleetEmbeddingsDoesNotMisrouteToWrongModelNode reproduces the
// req-af404f8a incident: before the hard
// eligibility filter, a request naming a model that only the (now-draining)
// Ollama node has could still land on a healthy non-Ollama node whose
// LoadedModels contains a completely different model - model presence was a
// scoring bonus (isModelWarm/computeNodeScore), never a hard prerequisite, so
// the vLLM node was selected anyway and returned a silent wrong-model 200.
// After the fix (isEligibleForModel in placement.go), a non-Ollama node with
// the wrong LoadedModels entry must never be selected for a request naming
// another model - the correct outcome here is "no eligible node" (503), not
// a misrouted 200.
func TestMixedFleetEmbeddingsDoesNotMisrouteToWrongModelNode(t *testing.T) {
	var hitOllama, hitVLLM bool
	h, _, _ := newMixedFleetHandler(t, "nomic-embed-text", "other-embed-model",
		func() { hitOllama = true },
		func() { hitVLLM = true },
	)

	// Drain the Ollama node (the only node that actually has the requested
	// model) - mirrors the live incident where the Ollama node became
	// unavailable/draining while a differently-modeled vLLM node stayed healthy.
	for _, n := range h.router.Nodes() {
		if n.Name == "ollama-node" {
			n.Lock()
			n.Draining = true
			n.Unlock()
		}
	}

	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","input":"hello"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if hitVLLM {
		t.Error("req-af404f8a regression: a healthy non-Ollama node with a different LoadedModels entry received a request for \"nomic-embed-text\" - it must be ineligible, not just lower-scored")
	}
	if hitOllama {
		t.Error("the draining Ollama node should not have received the request either")
	}
	if rec.Code == http.StatusOK {
		t.Errorf("expected no eligible node (503), got 200 with body=%s", rec.Body.String())
	}
}

// TestMixedFleetStickySessionDoesNotBypassModelEligibility verifies the
// sticky-session shortcut in Route ("sticky routing was
// specifically identified as another possible bypass") also honors the hard
// eligibility filter - a session pinned to a non-Ollama node must not be
// allowed to reuse that node for a model the node doesn't actually have.
func TestMixedFleetStickySessionDoesNotBypassModelEligibility(t *testing.T) {
	r := router.New(config.RoutingConfig{SessionAffinity: true}, []config.NodeConfig{
		{Name: "ollama-node", URL: "http://ollama.invalid", Runtime: "ollama"},
		{Name: "vllm-node", URL: "http://vllm.invalid", Runtime: "vllm"},
	}, nil)

	var vllmNode *router.NodeState
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		switch n.Name {
		case "ollama-node":
			n.LoadedModels = []router.ModelInfo{{Name: "nomic-embed-text"}}
		case "vllm-node":
			n.LoadedModels = []router.ModelInfo{{Name: "other-embed-model"}}
			vllmNode = n
		}
		n.Unlock()
	}
	if vllmNode == nil {
		t.Fatal("vllm-node not found")
	}

	// Directly route once for the vLLM node's own model with a session ID to
	// establish a sticky-session entry pinned to vllmNode.
	node, _, _ := r.Route("other-embed-model", "sess-1", "")
	if node != vllmNode {
		t.Fatalf("expected initial route to pin sticky session to vllm-node, got %v", node)
	}

	// Now route the SAME session for a model vllmNode does not have. The
	// sticky shortcut must not return vllmNode just because it's the pinned
	// node - it must fall through and re-evaluate eligibility.
	node, _, _ = r.Route("nomic-embed-text", "sess-1", "")
	if node == vllmNode {
		t.Error("sticky-session path returned a node ineligible for the requested model - eligibility must be re-checked even for a pinned session")
	}
}
