package proxy

// anthropic_hardening_test.go -- regression tests for the cloud translators:
// default stream handling, usage totals, truncated/errored streams, finish
// reasons, response size cap, SSE framing tolerance, and request mapping.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type anthropicRTFunc func(*http.Request) (*http.Response, error)

func (f anthropicRTFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// captureAnthropicBody sends body through the transport chain built by wrap
// and returns the translated Anthropic request the inner transport saw.
func captureAnthropicBody(t *testing.T, wrap func(inner http.RoundTripper) http.RoundTripper, body string) anthropicRequest {
	t.Helper()
	var seen []byte
	inner := anthropicRTFunc(func(r *http.Request) (*http.Response, error) {
		seen, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"x"}],"usage":{}}`)),
		}, nil
	})
	rt := wrap(inner)
	req, _ := http.NewRequest(http.MethodPost, "http://cloud.invalid/v1/chat/completions", strings.NewReader(body))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	var got anthropicRequest
	if err := json.Unmarshal(seen, &got); err != nil {
		t.Fatalf("inner saw invalid body %q: %v", seen, err)
	}
	return got
}

func TestAnthropicDefaultStream_OpenAIPathDefaultsFalse(t *testing.T) {
	got := captureAnthropicBody(t, func(in http.RoundTripper) http.RoundTripper {
		return &anthropicTransport{inner: in, apiKey: "k"}
	}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got.Stream {
		t.Error("absent stream on an OpenAI-path request must default to false")
	}
}

func TestAnthropicDefaultStream_OllamaPathDefaultsTrue(t *testing.T) {
	got := captureAnthropicBody(t, func(in http.RoundTripper) http.RoundTripper {
		return &translatingTransport{
			inner:        &anthropicTransport{inner: in, apiKey: "k"},
			origPath:     "/api/chat",
			clientModel:  "m",
			clientStream: true,
		}
	}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if !got.Stream {
		t.Error("absent stream on an Ollama /api path must default to true")
	}
}

func readAnthropicStream(t *testing.T, events string) (string, error) {
	t.Helper()
	out := translateAnthropicSSEToOpenAI(io.NopCloser(strings.NewReader(events)))
	raw, err := io.ReadAll(out)
	return string(raw), err
}

func TestAnthropicSSE_UsageChunkHasTotalTokens(t *testing.T) {
	events := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	body, err := readAnthropicStream(t, events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var total int64 = -1
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var c struct {
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c) == nil && c.Usage != nil {
			total = c.Usage.TotalTokens
		}
	}
	if total != 11 {
		t.Errorf("usage total_tokens = %d, want 11; body=%q", total, body)
	}
}

func TestAnthropicSSE_TruncatedStreamErrors(t *testing.T) {
	events := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n"
	body, err := readAnthropicStream(t, events)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF; body=%q", err, body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("truncated stream must not emit [DONE]: %q", body)
	}
}

func TestAnthropicSSE_ErrorEventSurfacesError(t *testing.T) {
	events := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	body, err := readAnthropicStream(t, events)
	if err == nil {
		t.Errorf("error event must close the stream with an error; body=%q", body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "overloaded_error") {
		t.Errorf("body should carry an OpenAI-style error chunk, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("error stream must not emit [DONE]: %q", body)
	}
}

func TestAnthropicSSE_NoSpaceAndCRLF(t *testing.T) {
	events := "data:{\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\r\n\r\n" +
		"data:{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\r\n\r\n" +
		"data: {\"type\":\"message_stop\"}\r\n\r\n"
	body, err := readAnthropicStream(t, events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, `"content":"ok"`) || !strings.Contains(body, "[DONE]") {
		t.Errorf("no-space/CRLF frames not translated: %q", body)
	}
}

func TestAnthropicSSE_FinishReasonChunk(t *testing.T) {
	events := "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	body, err := readAnthropicStream(t, events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fi := strings.Index(body, `"finish_reason":"length"`)
	ui := strings.Index(body, `"usage"`)
	if fi < 0 || ui < 0 || fi > ui {
		t.Errorf("want finish_reason length chunk before usage chunk, got %q", body)
	}
}

func TestAnthropicJSON_FinishReasonMapped(t *testing.T) {
	cases := map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls"}
	for in, want := range cases {
		src := `{"content":[{"type":"text","text":"x"}],"stop_reason":"` + in + `","usage":{}}`
		raw, _ := io.ReadAll(translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(src))))
		var got openAIChatResponse
		if err := json.Unmarshal(raw, &got); err != nil || len(got.Choices) != 1 {
			t.Fatalf("bad output %q: %v", raw, err)
		}
		if got.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %q -> %q, want %q", in, got.Choices[0].FinishReason, want)
		}
	}
}

func TestCloudResponseSizeCap(t *testing.T) {
	old := maxCloudResponseBytes
	maxCloudResponseBytes = 64
	defer func() { maxCloudResponseBytes = old }()

	big := `{"content":[{"type":"text","text":"` + strings.Repeat("a", 200) + `"}],"usage":{}}`
	raw, _ := io.ReadAll(translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(big))))
	if !strings.Contains(string(raw), `"error"`) || strings.Contains(string(raw), "aaaa") {
		t.Errorf("Anthropic oversize: want error body, got %q", raw)
	}
	oai := `{"choices":[{"message":{"content":"` + strings.Repeat("a", 200) + `"}}],"usage":{}}`
	raw, _ = io.ReadAll(translateJSONToNDJSON(io.NopCloser(strings.NewReader(oai)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"error"`) || strings.Contains(string(raw), "aaaa") {
		t.Errorf("NDJSON oversize: want error body, got %q", raw)
	}
	raw, _ = io.ReadAll(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(oai)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"error"`) || strings.Contains(string(raw), "aaaa") {
		t.Errorf("single oversize: want error body, got %q", raw)
	}
}

func TestOllamaSSE_NoSpaceAndCRLF(t *testing.T) {
	src := "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\ndata:[DONE]\r\n\r\n"
	raw, err := io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), `"content":"hi"`) || !strings.Contains(string(raw), `"done":true`) {
		t.Errorf("no-space/CRLF frames not translated: %q", raw)
	}
}

func TestTranslateOpenAIRequestToAnthropic_DeveloperRoleAndTemperatureClamp(t *testing.T) {
	body := []byte(`{"model":"m","temperature":1.7,"messages":[{"role":"developer","content":"rules"},{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	out, err := translateOpenAIRequestToAnthropic(body, false)
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.System != "rules\n\nsys" {
		t.Errorf("System = %q, want developer+system folded", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Errorf("Messages = %+v, want only the user message", got.Messages)
	}
	if got.Temperature == nil || *got.Temperature != 1 {
		t.Errorf("Temperature = %v, want clamped to 1", got.Temperature)
	}
}

func TestTranslateOpenAIRequestToAnthropic_EmptyMessagesIsArray(t *testing.T) {
	out, err := translateOpenAIRequestToAnthropic([]byte(`{"model":"m"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"messages":[]`) {
		t.Errorf("empty messages must marshal as [], got %s", out)
	}
}
