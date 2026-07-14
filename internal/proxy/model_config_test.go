package proxy

import (
	"encoding/json"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

// TestInjectModelDefaultsOllamaNativeFillsAbsentFields verifies configured
// load-time and inference-time defaults land in the "options" object, and
// system/keep_alive land at the top level, for an Ollama-native request.
func TestInjectModelDefaultsOllamaNativeFillsAbsentFields(t *testing.T) {
	cfg := store.ModelConfig{
		Model:       "llama3.3:70b",
		Node:        "gpu-node-01",
		NumCtx:      ip(8192),
		Temperature: fp(0.5),
		System:      strp("You are terse."),
		TTL:         ip(300),
	}
	body := []byte(`{"model":"llama3.3:70b","prompt":"hi"}`)
	out := injectModelDefaults(body, "ollama", cfg)

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	opts, ok := m["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("options missing or wrong type: %v", m["options"])
	}
	if opts["num_ctx"] != float64(8192) {
		t.Errorf("num_ctx = %v, want 8192", opts["num_ctx"])
	}
	if opts["temperature"] != 0.5 {
		t.Errorf("temperature = %v, want 0.5", opts["temperature"])
	}
	if m["system"] != "You are terse." {
		t.Errorf("system = %v", m["system"])
	}
	if m["keep_alive"] != float64(300) {
		t.Errorf("keep_alive = %v, want 300", m["keep_alive"])
	}
}

// TestInjectModelDefaultsEmptyRuntimeTreatedAsOllama verifies "" (the
// back-compat sentinel used by NodeRecord.Runtime for pre-existing nodes)
// is treated identically to "ollama".
func TestInjectModelDefaultsEmptyRuntimeTreatedAsOllama(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", NumCtx: ip(4096)}
	body := []byte(`{"model":"m","prompt":"hi"}`)
	out := injectModelDefaults(body, "", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	opts, ok := m["options"].(map[string]interface{})
	if !ok || opts["num_ctx"] != float64(4096) {
		t.Fatalf("empty runtime should behave like ollama, got: %v", m)
	}
}

// TestInjectModelDefaultsNeverOverwritesClientValue verifies a client-specified
// options field is left untouched even when a default is configured.
func TestInjectModelDefaultsNeverOverwritesClientValue(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", Temperature: fp(0.9)}
	body := []byte(`{"model":"m","options":{"temperature":0.1}}`)
	out := injectModelDefaults(body, "ollama", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	opts := m["options"].(map[string]interface{})
	if opts["temperature"] != 0.1 {
		t.Fatalf("temperature = %v, want client-specified 0.1 preserved", opts["temperature"])
	}
}

// TestInjectModelDefaultsOpenAICompatSkipsOllamaOnlyFields verifies /v1/*
// requests to a non-Ollama runtime only get strict-OpenAI-schema fields
// injected, not Ollama-specific load-time knobs like num_ctx.
func TestInjectModelDefaultsOpenAICompatSkipsOllamaOnlyFields(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", NumCtx: ip(4096), Temperature: fp(0.4)}
	body := []byte(`{"model":"m","messages":[]}`)
	out := injectModelDefaults(body, "tgi", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if _, ok := m["num_ctx"]; ok {
		t.Errorf("num_ctx leaked into OpenAI-compat body: %v", m)
	}
	if m["temperature"] != 0.4 {
		t.Errorf("temperature = %v, want 0.4", m["temperature"])
	}
}

// TestInjectModelDefaultsVLLMExtraFields verifies vLLM gets its own-named
// extras (repetition_penalty, not Ollama's repeat_penalty) beyond the
// strict OpenAI schema, and that a field TGI doesn't support (top_k) is
// still correctly withheld for TGI.
func TestInjectModelDefaultsVLLMExtraFields(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", TopK: ip(40), RepeatPenalty: fp(1.1)}

	vllmOut := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "vllm", cfg)
	var vm map[string]interface{}
	json.Unmarshal(vllmOut, &vm)
	if vm["top_k"] != float64(40) {
		t.Errorf("vllm top_k = %v, want 40", vm["top_k"])
	}
	if vm["repetition_penalty"] != 1.1 {
		t.Errorf("vllm repetition_penalty = %v, want 1.1", vm["repetition_penalty"])
	}
	if _, ok := vm["repeat_penalty"]; ok {
		t.Errorf("vllm body should use repetition_penalty, not repeat_penalty: %v", vm)
	}

	tgiOut := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "tgi", cfg)
	var tm map[string]interface{}
	json.Unmarshal(tgiOut, &tm)
	if _, ok := tm["top_k"]; ok {
		t.Errorf("tgi should not receive top_k (unsupported): %v", tm)
	}
	if _, ok := tm["repetition_penalty"]; ok {
		t.Errorf("tgi should not receive repetition_penalty (unsupported): %v", tm)
	}
}

// TestInjectModelDefaultsLlamaCppExtraFields verifies llama.cpp gets its own
// sampling extras (mirostat family, repeat_penalty under its own name).
func TestInjectModelDefaultsLlamaCppExtraFields(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", Mirostat: ip(2), RepeatPenalty: fp(1.2), TfsZ: fp(0.9)}
	out := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "llamacpp", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if m["mirostat"] != float64(2) {
		t.Errorf("llamacpp mirostat = %v, want 2", m["mirostat"])
	}
	if m["repeat_penalty"] != 1.2 {
		t.Errorf("llamacpp repeat_penalty = %v, want 1.2", m["repeat_penalty"])
	}
	if m["tfs_z"] != 0.9 {
		t.Errorf("llamacpp tfs_z = %v, want 0.9", m["tfs_z"])
	}
}

func strp(s string) *string { return &s }

// TestModelRateLimiterRPM verifies the pre-request rpm gate rejects once the
// per-minute cap is reached, and TPM blocks once recorded usage hits the cap.
func TestModelRateLimiterRPM(t *testing.T) {
	l := newModelRateLimiter()
	rpm := 2
	if !l.allow("m", "n", &rpm, nil) {
		t.Fatal("1st request should be allowed")
	}
	if !l.allow("m", "n", &rpm, nil) {
		t.Fatal("2nd request should be allowed")
	}
	if l.allow("m", "n", &rpm, nil) {
		t.Fatal("3rd request should be rejected (rpm=2)")
	}
}

func TestModelRateLimiterTPM(t *testing.T) {
	l := newModelRateLimiter()
	tpm := 100
	if !l.allow("m", "n", nil, &tpm) {
		t.Fatal("first request should be allowed before any tokens recorded")
	}
	l.recordTokens("m", "n", 150)
	if l.allow("m", "n", nil, &tpm) {
		t.Fatal("request should be rejected once recorded tokens exceed tpm cap")
	}
}

// TestModelRateLimiterPerNodeIsolation verifies the same model on two
// different nodes has independent rpm budgets — the core reason the
// limiter is now keyed by (model, node) rather than model alone.
func TestModelRateLimiterPerNodeIsolation(t *testing.T) {
	l := newModelRateLimiter()
	rpm := 1
	if !l.allow("m", "node-a", &rpm, nil) {
		t.Fatal("node-a 1st request should be allowed")
	}
	if l.allow("m", "node-a", &rpm, nil) {
		t.Fatal("node-a 2nd request should be rejected (rpm=1)")
	}
	if !l.allow("m", "node-b", &rpm, nil) {
		t.Fatal("node-b should have its own independent budget and allow its 1st request")
	}
}
