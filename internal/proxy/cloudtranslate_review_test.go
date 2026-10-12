package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestAnthropicJSON_MissingStopReasonInventsNoFinishReason(t *testing.T) {
	src := `{"content":[{"type":"text","text":"x"}],"usage":{}}`
	raw, _ := io.ReadAll(translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(src))))
	var got openAIChatResponse
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Choices) != 1 {
		t.Fatalf("bad output %q: %v", raw, err)
	}
	if got.Choices[0].FinishReason != "" {
		t.Errorf("finish_reason = %q, want none invented", got.Choices[0].FinishReason)
	}
}

func TestSanitizeLogText(t *testing.T) {
	in := "a\nb\rc\x07d\xffe\u2028f\u2029g"
	if got := sanitizeLogText(in, 100); got != "a b c d e f g" {
		t.Errorf("sanitize = %q", got)
	}
	long := strings.Repeat("é", 50)
	got := sanitizeLogText(long, 11)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") || len(got) > 11+3 {
		t.Errorf("bad truncation %q", got)
	}
	// Exactly max bytes is returned whole, with no suffix.
	if got := sanitizeLogText("abcde", 5); got != "abcde" {
		t.Errorf("exact-length input changed: %q", got)
	}
	if got := sanitizeLogText("abcdef", 5); got != "abcde..." {
		t.Errorf("one over max: %q", got)
	}
}

func TestStreamErrorChunk_EscapedSeparatorsLogOnOneLine(t *testing.T) {
	buf := captureLog(t)
	frames := []string{
		"data: {\"error\":{\"message\":\"first\\nline\"}}\n\n",
		"data: {\"error\":{\"message\":\"left\\u2028right\\u0007bell\"}}\n\n",
		"data: {\"error\":{\"message\":\"carriage\\rreturn\"}}\n\n",
		"data: {\"error\":{\"message\":\"" + strings.Repeat("x", 1000) + "\"}}\n\n",
	}
	for _, src := range frames {
		out := translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m")
		io.ReadAll(out) //nolint:errcheck
	}
	logged := buf.String()
	for _, want := range []string{"first line", "left right bell", "carriage return"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("sanitized provider message %q not logged: %q", want, logged)
		}
	}
	lines := strings.Split(strings.TrimSuffix(logged, "\n"), "\n")
	if len(lines) != 4 {
		t.Errorf("want one line per chunk (4), got %d: %q", len(lines), logged)
	}
	const marker = "upstream error chunk: "
	for _, line := range lines {
		i := strings.Index(line, marker)
		if i < 0 {
			t.Errorf("line without provider message: %q", line)
			continue
		}
		if n := len(line) - i - len(marker); n > maxLoggedProviderChunk+3 {
			t.Errorf("logged message not bounded: %d bytes", n)
		}
	}
	for _, bad := range []string{" ", "\x07", "\r"} {
		if strings.Contains(logged, bad) {
			t.Errorf("unsanitized %q in log %q", bad, logged)
		}
	}
}

func TestClampAnthropicTemperature_LogRateLimited(t *testing.T) {
	buf := captureLog(t)
	oldInterval := temperatureClampLogInterval
	temperatureClampLogInterval = time.Hour
	temperatureClampLastLog.Store(0)
	defer func() {
		temperatureClampLogInterval = oldInterval
		temperatureClampLastLog.Store(0)
	}()
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

// An Anthropic reply with no stop_reason carries no finish_reason in OpenAI
// form, but a client replying in Ollama format still gets done_reason "stop",
// the same default the stream path applies.
func TestAnthropicJSONToOllama_MissingStopReasonDefaultsDoneReason(t *testing.T) {
	src := `{"content":[{"type":"text","text":"x"}],"usage":{}}`
	openAI := translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(src)))
	raw, _ := io.ReadAll(translateJSONToSingleOllama(openAI, "/api/chat", "m"))
	if !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("done_reason default missing: %s", raw)
	}
}

func TestSanitizeLogText_ReplacesGenuineReplacementChar(t *testing.T) {
	if got := sanitizeLogText("a\xef\xbf\xbdb", 100); got != "a b" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestAnthropicJSONToOllama_Generate_MissingStopReasonDefaultsDoneReason(t *testing.T) {
	src := `{"content":[{"type":"text","text":"x"}],"usage":{}}`
	openAI := translateAnthropicJSONToOpenAI(io.NopCloser(strings.NewReader(src)))
	raw, _ := io.ReadAll(translateJSONToSingleOllama(openAI, "/api/generate", "m"))
	if !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("done_reason default missing on /api/generate: %s", raw)
	}
}
