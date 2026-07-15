package proxy

import (
	"encoding/json"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }
func boolp(v bool) *bool    { return &v }

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
// sampling extras (mirostat family, repeat_penalty under its own name, and
// num_keep injected under llama.cpp's own "n_keep" wire name).
func TestInjectModelDefaultsLlamaCppExtraFields(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", Mirostat: ip(2), RepeatPenalty: fp(1.2), NumKeep: ip(16)}
	out := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "llamacpp", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if m["mirostat"] != float64(2) {
		t.Errorf("llamacpp mirostat = %v, want 2", m["mirostat"])
	}
	if m["repeat_penalty"] != 1.2 {
		t.Errorf("llamacpp repeat_penalty = %v, want 1.2", m["repeat_penalty"])
	}
	if m["n_keep"] != float64(16) {
		t.Errorf("llamacpp n_keep = %v, want 16", m["n_keep"])
	}
	if _, ok := m["num_keep"]; ok {
		t.Errorf("llamacpp body should use n_keep, not num_keep: %v", m)
	}
}

// TestInjectModelDefaultsLlamaCppSamplerExtras verifies llama.cpp's DRY/XTC/
// logit_bias/n_probs/min_keep sampling extras, which llama.cpp's server
// README documents as also accepted on its OpenAI-compatible endpoint.
func TestInjectModelDefaultsLlamaCppSamplerExtras(t *testing.T) {
	cfg := store.ModelConfig{
		Model: "m", Node: "n",
		DryMultiplier: fp(0.8), DryBase: fp(1.75), DryAllowedLength: ip(2), DryPenaltyLastN: ip(64),
		XtcProbability: fp(0.5), XtcThreshold: fp(0.1),
		NProbs: ip(5), MinKeep: ip(1),
		LogitBias: map[string]float64{"1234": -5},
	}
	out := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "llamacpp", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	checks := map[string]float64{
		"dry_multiplier": 0.8, "dry_base": 1.75, "dry_allowed_length": 2, "dry_penalty_last_n": 64,
		"xtc_probability": 0.5, "xtc_threshold": 0.1, "n_probs": 5, "min_keep": 1,
	}
	for key, want := range checks {
		if m[key] != want {
			t.Errorf("llamacpp %s = %v, want %v", key, m[key], want)
		}
	}
	lb, ok := m["logit_bias"].(map[string]interface{})
	if !ok || lb["1234"] != -5.0 {
		t.Errorf("llamacpp logit_bias = %v, want {1234: -5}", m["logit_bias"])
	}
}

// TestInjectModelDefaultsVLLMSamplerExtras verifies vLLM's extended sampling
// fields beyond top_k/min_p/repetition_penalty (its ChatCompletionRequest
// schema accepts these too).
func TestInjectModelDefaultsVLLMSamplerExtras(t *testing.T) {
	cfg := store.ModelConfig{
		Model: "m", Node: "n",
		LengthPenalty: fp(1.1), StopTokenIDs: []int{100, 200},
		IncludeStopStrInOutput: boolp(true), IgnoreEOS: boolp(true),
		MinTokens: ip(10), SkipSpecialTokens: boolp(false), TruncatePromptTokens: ip(2048),
	}
	out := injectModelDefaults([]byte(`{"model":"m","messages":[]}`), "vllm", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if m["length_penalty"] != 1.1 {
		t.Errorf("vllm length_penalty = %v, want 1.1", m["length_penalty"])
	}
	if m["ignore_eos"] != true {
		t.Errorf("vllm ignore_eos = %v, want true", m["ignore_eos"])
	}
	if m["min_tokens"] != float64(10) {
		t.Errorf("vllm min_tokens = %v, want 10", m["min_tokens"])
	}
	if m["skip_special_tokens"] != false {
		t.Errorf("vllm skip_special_tokens = %v, want false", m["skip_special_tokens"])
	}
	if m["truncate_prompt_tokens"] != float64(2048) {
		t.Errorf("vllm truncate_prompt_tokens = %v, want 2048", m["truncate_prompt_tokens"])
	}
	ids, ok := m["stop_token_ids"].([]interface{})
	if !ok || len(ids) != 2 || ids[0] != float64(100) {
		t.Errorf("vllm stop_token_ids = %v, want [100, 200]", m["stop_token_ids"])
	}
}

// TestInjectModelDefaultsOllamaDeadFieldsNotInjected verifies fields that no
// longer exist in Ollama's current Options/Runner structs (removed 2026-07)
// are simply absent from the struct - nothing to inject, nothing that could
// silently no-op. The new real Ollama fields (num_keep, main_gpu,
// draft_num_predict) DO get injected.
func TestInjectModelDefaultsOllamaDeadFieldsNotInjected(t *testing.T) {
	cfg := store.ModelConfig{
		Model: "m", Node: "n",
		NumKeep: ip(8), MainGPU: ip(1), DraftNumPredict: ip(4),
	}
	out := injectModelDefaults([]byte(`{"model":"m","prompt":"hi"}`), "ollama", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	opts := m["options"].(map[string]interface{})
	if opts["num_keep"] != float64(8) {
		t.Errorf("ollama num_keep = %v, want 8", opts["num_keep"])
	}
	if opts["main_gpu"] != float64(1) {
		t.Errorf("ollama main_gpu = %v, want 1", opts["main_gpu"])
	}
	if opts["draft_num_predict"] != float64(4) {
		t.Errorf("ollama draft_num_predict = %v, want 4", opts["draft_num_predict"])
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
// different nodes has independent rpm budgets - the core reason the
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

// TestInjectModelDefaultsOpenAICompatSystemPromptInjection verifies a
// configured system prompt is prepended as a leading system-role message on
// a chat-shaped OpenAI-compatible request, for every non-Ollama runtime.
func TestInjectModelDefaultsOpenAICompatSystemPromptInjection(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", System: strp("Always answer BANANA.")}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := injectModelDefaults(body, "vllm", cfg)

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	msgs, ok := m["messages"].([]interface{})
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 entries (system + original user)", m["messages"])
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "Always answer BANANA." {
		t.Fatalf("first message = %v, want the configured system prompt", first)
	}
	second := msgs[1].(map[string]interface{})
	if second["role"] != "user" || second["content"] != "hi" {
		t.Fatalf("original user message was mutated: %v", second)
	}
}

// TestInjectModelDefaultsOpenAICompatSystemPromptNeverOverwritesClient
// verifies a client-supplied system message is left alone even when a
// default is configured.
func TestInjectModelDefaultsOpenAICompatSystemPromptNeverOverwritesClient(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", System: strp("configured default")}
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"client system prompt"},{"role":"user","content":"hi"}]}`)
	out := injectModelDefaults(body, "tgi", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want unchanged 2 entries (no system message inserted)", msgs)
	}
	first := msgs[0].(map[string]interface{})
	if first["content"] != "client system prompt" {
		t.Fatalf("client system prompt was overwritten: %v", first)
	}
}

// TestInjectModelDefaultsOpenAICompatSystemPromptNoMessagesArray verifies a
// legacy /v1/completions-style body (no "messages" array) is left untouched
// by the system-prompt injection - there's no place to carry a system role
// in that schema.
func TestInjectModelDefaultsOpenAICompatSystemPromptNoMessagesArray(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Node: "n", System: strp("configured default")}
	body := []byte(`{"model":"m","prompt":"hi"}`)
	out := injectModelDefaults(body, "llamacpp", cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if _, ok := m["messages"]; ok {
		t.Fatalf("messages array should not be fabricated: %v", m)
	}
	if _, ok := m["system"]; ok {
		t.Fatalf("bare 'system' field should not be injected for non-Ollama runtimes: %v", m)
	}
}
