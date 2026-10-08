package proxy

// anthropic_review_test.go -- further regression tests for the cloud
// translators: error and done_reason propagation to Ollama clients, request
// mapping edge cases, and the real handler path.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

// ollamaChainStream runs an Anthropic SSE body through the full
// translatingTransport + anthropicTransport chain and returns the Ollama
// output and any read error.
func ollamaChainStream(t *testing.T, path, sse string) (string, error) {
	t.Helper()
	inner := anthropicRTFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	rt := &translatingTransport{
		inner:        &anthropicTransport{inner: inner, apiKey: "k"},
		origPath:     path,
		clientModel:  "m",
		clientStream: true,
	}
	req, _ := http.NewRequest(http.MethodPost, "http://cloud.invalid/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	raw, rerr := io.ReadAll(resp.Body)
	return string(raw), rerr
}

func TestOllamaChain_AnthropicErrorEventForwardsMessage(t *testing.T) {
	sse := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	body, err := ollamaChainStream(t, "/api/chat", sse)
	if err == nil {
		t.Error("want a read error after the error event")
	}
	if !strings.Contains(body, "Overloaded") {
		t.Errorf("Ollama error line should carry the upstream message, got %q", body)
	}
	if strings.Contains(body, "ended unexpectedly") {
		t.Errorf("generic message must not replace the upstream detail: %q", body)
	}
}

func TestOllamaChain_MaxTokensShowsDoneReasonLength(t *testing.T) {
	sse := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	for _, path := range []string{"/api/chat", "/api/generate"} {
		body, err := ollamaChainStream(t, path, sse)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", path, err)
		}
		if !strings.Contains(body, `"done_reason":"length"`) {
			t.Errorf("%s: want done_reason length on the final line, got %q", path, body)
		}
	}
}

func TestOllamaJSON_DoneReasonPassedThrough(t *testing.T) {
	src := `{"choices":[{"message":{"content":"x"},"finish_reason":"length"}],"usage":{}}`
	raw, _ := io.ReadAll(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(src)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"done_reason":"length"`) {
		t.Errorf("single: %q", raw)
	}
	raw, _ = io.ReadAll(translateJSONToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/generate", "m"))
	if !strings.Contains(string(raw), `"done_reason":"length"`) {
		t.Errorf("ndjson: %q", raw)
	}
}

func TestOllamaSSE_ErrorChunkForwarded(t *testing.T) {
	src := "data: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	raw, err := io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m"))
	if err == nil {
		t.Error("want an error")
	}
	if !strings.Contains(string(raw), "Overloaded") {
		t.Errorf("want upstream message in error line, got %q", raw)
	}
}

func TestSSEData(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"data: {\"a\":1}", "{\"a\":1}", true},
		{"data:{\"a\":1}", "{\"a\":1}", true},
		{"data:", "", true},
		{"data: ", "", true},
		{"event: error", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := sseData(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("sseData(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSSE_EmptyDataPayloadIgnored(t *testing.T) {
	a := "data:\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	body, err := readAnthropicStream(t, a)
	if err != nil || !strings.Contains(body, `"content":"ok"`) || !strings.Contains(body, "[DONE]") {
		t.Errorf("anthropic: err=%v body=%q", err, body)
	}
	o := "data:\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	raw, err := io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader(o)), "/api/chat", "m"))
	if err != nil || !strings.Contains(string(raw), `"done":true`) {
		t.Errorf("ollama: err=%v body=%q", err, raw)
	}
}

func TestAnthropicExplicitStreamFalseOnOllamaPath(t *testing.T) {
	got := captureAnthropicBody(t, func(in http.RoundTripper) http.RoundTripper {
		return &translatingTransport{inner: &anthropicTransport{inner: in, apiKey: "k"}, origPath: "/api/chat", clientModel: "m"}
	}, `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if got.Stream {
		t.Error("explicit stream:false must be honored on an Ollama path")
	}
}

func TestAnthropicFinishReasonEdgeMapping(t *testing.T) {
	for in, want := range map[string]string{"refusal": "content_filter", "something_new": "stop", "": ""} {
		if got := openAIFinishReason(in); got != want {
			t.Errorf("openAIFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslateOpenAIRequestToAnthropic_TemperatureCases(t *testing.T) {
	cases := []struct {
		in   string
		want *float64
	}{
		{`"temperature":0,`, fp(0)},
		{``, nil},
		{`"temperature":-0.5,`, fp(0)},
		{`"temperature":0.3,`, fp(0.3)},
	}
	for _, c := range cases {
		out, err := translateOpenAIRequestToAnthropic([]byte(`{`+c.in+`"model":"m"}`), false)
		if err != nil {
			t.Fatal(err)
		}
		var got anthropicRequest
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if (got.Temperature == nil) != (c.want == nil) || (c.want != nil && *got.Temperature != *c.want) {
			t.Errorf("%q: temperature = %v, want %v", c.in, got.Temperature, c.want)
		}
	}
}

func TestTranslateOpenAIRequestToAnthropic_EmptySystemSkipped(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":""},{"role":"developer","content":"rules"},{"role":"system","content":""},{"role":"user","content":"hi"}]}`
	out, err := translateOpenAIRequestToAnthropic([]byte(body), false)
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.System != "rules" {
		t.Errorf("System = %q, want %q (no stray separators)", got.System, "rules")
	}
}

func TestTranslateOpenAIRequestToAnthropic_EmptyMessagesArray(t *testing.T) {
	out, err := translateOpenAIRequestToAnthropic([]byte(`{"model":"m","messages":[]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"messages":[]`) {
		t.Errorf("got %s", out)
	}
}

// TestCloudFallbackAnthropicV1DefaultsToNonStreaming drives the real handler
// path: an OpenAI-path request without "stream" must reach Anthropic with
// stream false and get a JSON reply.
func TestCloudFallbackAnthropicV1DefaultsToNonStreaming(t *testing.T) {
	var gotStream *bool
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b struct {
			Stream *bool `json:"stream"`
		}
		json.Unmarshal(raw, &b) //nolint:errcheck
		gotStream = b.Stream
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer cloud.Close()

	h, _ := newAnthropicOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotStream == nil || *gotStream {
		t.Errorf("upstream stream = %v, want false", gotStream)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Errorf("want a JSON reply, got %q (%s)", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestInjectModelDefaults_SystemEdgeCases(t *testing.T) {
	sys := "be terse"
	empty := ""

	// messages is JSON null: left unchanged.
	in := `{"model":"m","messages":null}`
	if out := injectModelDefaults([]byte(in), "ollama", store.ModelConfig{System: &sys}); !bytes.Contains(out, []byte(`"messages":null`)) {
		t.Errorf("null messages must stay null: %s", out)
	}
	// Empty profile system: nothing prepended.
	in = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for _, rt := range []string{"ollama", "vllm"} {
		out := injectModelDefaults([]byte(in), rt, store.ModelConfig{System: &empty})
		if bytes.Contains(out, []byte(`"role":"system"`)) {
			t.Errorf("%s: empty system must not be injected: %s", rt, out)
		}
	}
	// OpenAI-compat branch still prepends.
	out := injectModelDefaults([]byte(in), "vllm", store.ModelConfig{System: &sys})
	if !bytes.Contains(out, []byte(`"role":"system"`)) || !bytes.Contains(out, []byte(`"content":"be terse"`)) {
		t.Errorf("vllm should get a system message: %s", out)
	}
}
