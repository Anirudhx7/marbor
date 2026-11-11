package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamChunks returns the parsed JSON payloads of a streaming chat response.
func streamChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		out = append(out, m)
	}
	return out
}

func chat(t *testing.T, body string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleOpenAIChatCompletion(rec, req, "n", "m", 0)
	return streamChunks(t, rec.Body.String())
}

func TestStreamIncludesUsageChunkOnlyWhenRequested(t *testing.T) {
	with := chat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
	last := with[len(with)-1]
	usage, ok := last["usage"].(map[string]any)
	if !ok {
		t.Fatalf("last chunk must carry usage: %v", last)
	}
	if choices, _ := last["choices"].([]any); len(choices) != 0 {
		t.Fatalf("usage chunk must have empty choices: %v", last)
	}
	if usage["prompt_tokens"].(float64) <= 0 {
		t.Fatalf("prompt_tokens missing: %v", usage)
	}

	without := chat(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	for _, c := range without {
		if _, has := c["usage"]; has {
			t.Fatalf("usage chunk must not appear unless requested: %v", c)
		}
	}
}

func TestStreamUsageReportsCachedTokensUnderSimKV(t *testing.T) {
	t.Setenv("SIM_KV", "1")
	t.Setenv("SIM_BASE_MS", "0")
	t.Setenv("SIM_PREFILL_US_PER_TOKEN", "1")
	t.Setenv("SIM_DECODE_MS_PER_TOKEN", "0")
	prev := simEngine
	simEngine = newKVSimFromEnv()
	defer func() { simEngine = prev }()
	words := strings.Repeat("alpha beta gamma delta ", 40)
	body := `{"model":"m","messages":[{"role":"user","content":"` + words + `"}],"stream":true,"stream_options":{"include_usage":true}}`
	cached := func() float64 {
		chunks := chat(t, body)
		usage := chunks[len(chunks)-1]["usage"].(map[string]any)
		return usage["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64)
	}
	if first := cached(); first != 0 {
		t.Fatalf("first request must be uncached, got %v", first)
	}
	if second := cached(); second <= 0 {
		t.Fatalf("repeat request must report cached tokens, got %v", second)
	}
}
