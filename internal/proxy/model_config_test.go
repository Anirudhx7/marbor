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
		NumCtx:      ip(8192),
		Temperature: fp(0.5),
		System:      strp("You are terse."),
		TTL:         ip(300),
	}
	body := []byte(`{"model":"llama3.3:70b","prompt":"hi"}`)
	out := injectModelDefaults(body, true, cfg)

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

// TestInjectModelDefaultsNeverOverwritesClientValue verifies a client-specified
// options field is left untouched even when a default is configured.
func TestInjectModelDefaultsNeverOverwritesClientValue(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", Temperature: fp(0.9)}
	body := []byte(`{"model":"m","options":{"temperature":0.1}}`)
	out := injectModelDefaults(body, true, cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	opts := m["options"].(map[string]interface{})
	if opts["temperature"] != 0.1 {
		t.Fatalf("temperature = %v, want client-specified 0.1 preserved", opts["temperature"])
	}
}

// TestInjectModelDefaultsOpenAICompatSkipsOllamaOnlyFields verifies /v1/*
// requests only get OpenAI-schema fields injected, not Ollama-specific
// load-time knobs like num_ctx.
func TestInjectModelDefaultsOpenAICompatSkipsOllamaOnlyFields(t *testing.T) {
	cfg := store.ModelConfig{Model: "m", NumCtx: ip(4096), Temperature: fp(0.4)}
	body := []byte(`{"model":"m","messages":[]}`)
	out := injectModelDefaults(body, false, cfg)

	var m map[string]interface{}
	json.Unmarshal(out, &m)
	if _, ok := m["num_ctx"]; ok {
		t.Errorf("num_ctx leaked into OpenAI-compat body: %v", m)
	}
	if m["temperature"] != 0.4 {
		t.Errorf("temperature = %v, want 0.4", m["temperature"])
	}
}

func strp(s string) *string { return &s }

// TestModelRateLimiterRPM verifies the pre-request rpm gate rejects once the
// per-minute cap is reached, and TPM blocks once recorded usage hits the cap.
func TestModelRateLimiterRPM(t *testing.T) {
	l := newModelRateLimiter()
	rpm := 2
	if !l.allow("m", &rpm, nil) {
		t.Fatal("1st request should be allowed")
	}
	if !l.allow("m", &rpm, nil) {
		t.Fatal("2nd request should be allowed")
	}
	if l.allow("m", &rpm, nil) {
		t.Fatal("3rd request should be rejected (rpm=2)")
	}
}

func TestModelRateLimiterTPM(t *testing.T) {
	l := newModelRateLimiter()
	tpm := 100
	if !l.allow("m", nil, &tpm) {
		t.Fatal("first request should be allowed before any tokens recorded")
	}
	l.recordTokens("m", 150)
	if l.allow("m", nil, &tpm) {
		t.Fatal("request should be rejected once recorded tokens exceed tpm cap")
	}
}
