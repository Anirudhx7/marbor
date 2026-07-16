package proxy

// cloudtranslate_anthropic_test.go -- tests for the OpenAI-shape <->
// Anthropic Messages-shape translation layer used when cfg.Cloud provider is
// "anthropic".

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// newAnthropicOnlyHandler mirrors newCloudOnlyHandler (cloudtranslate_test.go)
// but with an anthropic-provider cloud, so cloud fallback exercises
// anthropicTransport.
func newAnthropicOnlyHandler(t *testing.T, cloudURL string) (*Handler, *admin.Server) {
	t.Helper()
	cloud := config.CloudProvider{
		Name:            "test-anthropic",
		Provider:        "anthropic",
		BaseURL:         cloudURL,
		APIKey:          "test-anthropic-key",
		DefaultModel:    "claude-3-5-sonnet-20241022",
		CostPer1KTokens: 0.003,
		Enabled:         true,
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, []config.CloudProvider{cloud})
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	return NewHandler(r, a, nil), a
}

// ------------------------------------------------------------------
// Request translation unit tests
// ------------------------------------------------------------------

func TestTranslateOpenAIRequestToAnthropic_ChatWithSystem(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],"stream":true,"temperature":0.5}`)
	out := translateOpenAIRequestToAnthropic(body, true)

	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v (%s)", err, out)
	}
	if got.System != "be terse" {
		t.Errorf("System = %q, want %q", got.System, "be terse")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v, want single user message 'hi' (system message must be pulled out)", got.Messages)
	}
	if !got.Stream {
		t.Error("Stream = false, want true")
	}
	if got.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want default %d", got.MaxTokens, anthropicDefaultMaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.5 {
		t.Errorf("Temperature = %v, want 0.5", got.Temperature)
	}
}

func TestTranslateOpenAIRequestToAnthropic_MaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256}`)
	out := translateOpenAIRequestToAnthropic(body, false)

	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v", err)
	}
	if got.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d, want 256 (from max_completion_tokens)", got.MaxTokens)
	}
}

func TestTranslateOpenAIRequestToAnthropic_PromptOnly(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","prompt":"hello world","stream":false}`)
	out := translateOpenAIRequestToAnthropic(body, false)

	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hello world" {
		t.Errorf("Messages = %+v, want single user message from prompt", got.Messages)
	}
}

// ------------------------------------------------------------------
// Response translation unit tests
// ------------------------------------------------------------------

func TestTranslateAnthropicJSONToOpenAI(t *testing.T) {
	anthropicResp := `{"content":[{"type":"text","text":"hello there"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":22}}`
	out := translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(anthropicResp)))
	raw, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("read translated body: %v", err)
	}

	var got openAIChatResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("translated body is not valid OpenAI-shape JSON: %v (%s)", err, raw)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "hello there" {
		t.Errorf("Choices = %+v, want single choice with content 'hello there'", got.Choices)
	}
	if got.Choices[0].FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want end_turn", got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 22 || got.Usage.TotalTokens != 33 {
		t.Errorf("Usage = %+v, want prompt=11 completion=22 total=33", got.Usage)
	}
}

func TestTranslateAnthropicSSEToOpenAI(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"

	out := translateAnthropicSSEToOpenAI(io.NopCloser(strings.NewReader(events)))
	raw, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("read translated SSE: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("translated SSE missing [DONE] sentinel: %q", body)
	}

	var contents []string
	var sawUsage bool
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk not valid JSON: %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				contents = append(contents, c.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			sawUsage = true
			if chunk.Usage.PromptTokens != 9 || chunk.Usage.CompletionTokens != 2 {
				t.Errorf("usage chunk = %+v, want prompt=9 completion=2", chunk.Usage)
			}
		}
	}
	if got := strings.Join(contents, ""); got != "Hello" {
		t.Errorf("assembled content = %q, want %q", got, "Hello")
	}
	if !sawUsage {
		t.Error("no usage chunk emitted before [DONE]")
	}
}

// ------------------------------------------------------------------
// End-to-end: cloud fallback through an anthropic provider
// ------------------------------------------------------------------

// TestCloudFallbackAnthropicAPIChatYieldsOllamaNDJSON verifies a /api/chat
// request that falls back to an anthropic-provider cloud reaches
// /v1/messages with x-api-key auth and a Messages-shaped body, and that the
// client still receives Ollama NDJSON.
func TestCloudFallbackAnthropicAPIChatYieldsOllamaNDJSON(t *testing.T) {
	var gotPath string
	var gotAPIKey string
	var gotVersion string
	var gotAuth string
	var gotReqBody anthropicRequest

	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotReqBody) //nolint:errcheck

		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock anthropic ResponseWriter does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		}
		for _, ev := range events {
			io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
	}))
	defer cloud.Close()

	h, _ := newAnthropicOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"llama3","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"stream":true}`))
	req := httptest.NewRequest("POST", "/api/chat", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if gotPath != "/v1/messages" {
		t.Errorf("cloud received path %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "test-anthropic-key" {
		t.Errorf("x-api-key = %q, want test-anthropic-key", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header missing")
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (anthropic uses x-api-key)", gotAuth)
	}
	if gotReqBody.System != "be brief" {
		t.Errorf("outbound System = %q, want 'be brief'", gotReqBody.System)
	}
	if len(gotReqBody.Messages) != 1 || gotReqBody.Messages[0].Content != "hi" {
		t.Errorf("outbound Messages = %+v, want single user message 'hi'", gotReqBody.Messages)
	}

	respBody := rec.Body.String()
	if strings.Contains(respBody, "data: ") || strings.Contains(respBody, "[DONE]") {
		t.Errorf("client received untranslated SSE: %q", respBody)
	}

	lines := parseNDJSON(t, respBody)
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want >= 2", len(lines))
	}
	last := lines[len(lines)-1]
	if last["done"] != true {
		t.Errorf("last line done = %v, want true", last["done"])
	}
	if last["eval_count"] != float64(2) {
		t.Errorf("eval_count = %v, want 2", last["eval_count"])
	}
	if last["prompt_eval_count"] != float64(3) {
		t.Errorf("prompt_eval_count = %v, want 3", last["prompt_eval_count"])
	}
}

// TestCloudFallbackAnthropicEmbeddingsUnsupported verifies /api/embeddings
// against an anthropic-only provider (no fallback) returns 501, since
// Anthropic has no embeddings endpoint to translate to.
func TestCloudFallbackAnthropicEmbeddingsUnsupported(t *testing.T) {
	h, _ := newAnthropicOnlyHandler(t, "http://localhost:1")
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"llama3","input":"hi"}`))
	req := httptest.NewRequest("POST", "/api/embeddings", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
}
