package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func errResp(status int, body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", "30")
	h.Set("Content-Length", "999")
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: h,
		ContentLength: 999, Body: io.NopCloser(strings.NewReader(body)),
	}
}

func TestConvertOllamaErrorBody_KeepsRetryAfter(t *testing.T) {
	for _, status := range []int{429, 503} {
		resp := errResp(status, `{"error":{"message":"slow down"}}`)
		convertOllamaErrorBody(resp)
		if got := resp.Header.Get("Retry-After"); got != "30" {
			t.Errorf("status %d: Retry-After = %q, want 30", status, got)
		}
		if resp.Header.Get("Content-Length") != "" {
			t.Errorf("status %d: stale Content-Length kept", status)
		}
		if resp.StatusCode != status {
			t.Errorf("status changed to %d", resp.StatusCode)
		}
	}
}

func TestCapCloudResponse_SynthesizedBadGatewayDropsRetryAfter(t *testing.T) {
	old := maxCloudResponseBytes
	maxCloudResponseBytes = 8
	defer func() { maxCloudResponseBytes = old }()
	resp := errResp(503, strings.Repeat("x", 100))
	resp.ContentLength = -1
	if capCloudResponse(resp, true) {
		t.Fatal("expected oversized body to be rejected")
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("Retry-After must be dropped on a synthesized 502")
	}
}

func TestUpstreamErrorMessage_Fallbacks(t *testing.T) {
	if got := upstreamErrorMessage(json.RawMessage(`{}`), "upstream error"); got != "upstream error" {
		t.Errorf("empty object: got %q", got)
	}
	if got := upstreamErrorMessage(json.RawMessage(`""`), "x"); got != "x" {
		t.Errorf("empty string: got %q", got)
	}
	if got := upstreamErrorMessage(json.RawMessage(`"boom"`), "x"); got != "boom" {
		t.Errorf("string: got %q", got)
	}
}

func TestConvertOllamaErrorBody_EmptyErrorObjectUsesNeutralFallback(t *testing.T) {
	resp := errResp(500, `{"error":{}}`)
	convertOllamaErrorBody(resp)
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "upstream stream") {
		t.Errorf("non-stream error mentions stream: %s", raw)
	}
	if !strings.Contains(string(raw), "upstream error") {
		t.Errorf("want neutral fallback, got %s", raw)
	}
}

func TestConvertOllamaErrorBody_NonErrorBodyUntouched(t *testing.T) {
	for _, body := range []string{`{"ok":true}`, `{"error":null}`, `not json`} {
		resp := errResp(500, body)
		convertOllamaErrorBody(resp)
		raw, _ := io.ReadAll(resp.Body)
		if string(raw) != body {
			t.Errorf("body %q rewritten to %q", body, raw)
		}
		if resp.Header.Get("Retry-After") != "30" {
			t.Errorf("headers modified for untouched body %q", body)
		}
	}
}

func TestStreamErrorChunk_FallbackStillStreamWording(t *testing.T) {
	src := "data: {\"error\":{}}\n\n"
	out := translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m")
	raw, _ := io.ReadAll(out)
	if !strings.Contains(string(raw), "upstream stream error") {
		t.Errorf("stream path lost its wording: %s", raw)
	}
}

func TestStreamErrorChunk_LoggedOnceTruncatedAndSanitized(t *testing.T) {
	buf := captureLog(t)
	long := strings.Repeat("é", 400)
	src := "data: {\"error\":{\"message\":\"" + long + "\\nSECOND\\u0007LINE\"}}\n\n"
	out := translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m")
	io.ReadAll(out) //nolint:errcheck
	logged := buf.String()
	if n := strings.Count(logged, "é"); n == 0 || n > 130 {
		t.Errorf("logged chunk not bounded: %d runes of payload", n)
	}
	if strings.Contains(strings.TrimSuffix(logged, "\n"), "\n") {
		t.Errorf("log has embedded newlines: %q", logged)
	}
	if strings.ContainsRune(logged, 7) {
		t.Error("control character not stripped")
	}
	if c := strings.Count(logged, "cloud stream failed"); c != 1 {
		t.Errorf("logged %d times, want 1", c)
	}
}

func TestNonStreamDoneReasonDefaultsToStop(t *testing.T) {
	body := `{"choices":[{"message":{"content":"hi"}}],"usage":{}}`
	raw, _ := io.ReadAll(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(body)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("single chat: %s", raw)
	}
	raw, _ = io.ReadAll(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(body)), "/api/generate", "m"))
	if !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("single generate: %s", raw)
	}
	raw, _ = io.ReadAll(translateJSONToNDJSON(io.NopCloser(strings.NewReader(body)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("ndjson: %s", raw)
	}
	lenBody := `{"choices":[{"message":{"content":"hi"},"finish_reason":"length"}],"usage":{}}`
	raw, _ = io.ReadAll(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(lenBody)), "/api/chat", "m"))
	if !strings.Contains(string(raw), `"done_reason":"length"`) {
		t.Errorf("explicit reason lost: %s", raw)
	}
}

func TestAnthropicJSON_MissingStopReasonDefaultsToStop(t *testing.T) {
	src := `{"content":[{"type":"text","text":"x"}],"usage":{}}`
	raw, _ := io.ReadAll(translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(src))))
	var got openAIChatResponse
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Choices) != 1 {
		t.Fatalf("bad output %q: %v", raw, err)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
}

func TestClampAnthropicTemperature_LogRateLimited(t *testing.T) {
	buf := captureLog(t)
	oldInterval := temperatureClampLogInterval
	temperatureClampLogInterval = time.Hour
	temperatureClampLastLog.Store(0)
	defer func() { temperatureClampLogInterval = oldInterval }()
	for i := 0; i < 5; i++ {
		v := 1.5 + float64(i)/10
		if got := clampAnthropicTemperature(&v); *got != 1 {
			t.Fatalf("clamp = %v", *got)
		}
	}
	if c := strings.Count(buf.String(), "clamped"); c != 1 {
		t.Errorf("logged %d times, want 1", c)
	}
	in := 0.5
	clampAnthropicTemperature(&in)
	if c := strings.Count(buf.String(), "clamped"); c != 1 {
		t.Error("in-range value must not log")
	}
}
