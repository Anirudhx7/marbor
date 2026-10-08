package proxy

// anthropic_round2_test.go -- regression tests for synthetic error response
// headers, options null handling, mid-stream failure logging, Ollama-shaped
// upstream errors and done_reason defaults.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestCapCloudResponse_ResetsBodyDescribingHeaders(t *testing.T) {
	old := maxCloudResponseBytes
	maxCloudResponseBytes = 16
	defer func() { maxCloudResponseBytes = old }()

	resp := &http.Response{
		StatusCode:    200,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(strings.Repeat("a", 100))),
		ContentLength: -1,
	}
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("Content-Length", "100")
	resp.Header.Set("Content-Range", "bytes 0-99/100")
	resp.Header.Set("ETag", `"x"`)
	resp.Header.Set("Retry-After", "5")
	if capCloudResponse(resp, false) {
		t.Fatal("oversize must fail")
	}
	for _, h := range []string{"Content-Encoding", "Content-Length", "Content-Range", "ETag", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("header %s = %q must be removed from the synthetic 502", h, v)
		}
	}
	if resp.StatusCode != 502 || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// nil header must not panic.
	resp2 := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", 100))), ContentLength: -1}
	if capCloudResponse(resp2, true) || resp2.StatusCode != 502 {
		t.Errorf("nil-header response: status %d", resp2.StatusCode)
	}
}

func TestInjectModelDefaults_OptionsNullIsAbsent(t *testing.T) {
	temp := 0.5
	cfg := store.ModelConfig{Temperature: &temp}

	out := injectModelDefaults([]byte(`{"model":"m","options":null}`), "ollama", cfg)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(top["options"], []byte(`"temperature":0.5`)) {
		t.Errorf("null options should get profile defaults: %s", out)
	}
	for _, raw := range []string{`"bogus"`, `[1]`, `5`} {
		out := injectModelDefaults([]byte(`{"model":"m","options":`+raw+`}`), "ollama", cfg)
		if err := json.Unmarshal(out, &top); err != nil {
			t.Fatal(err)
		}
		if string(top["options"]) != raw {
			t.Errorf("options %s must be left alone, got %s", raw, out)
		}
	}
}

func TestSSEFailure_LogsCauseAndDistinguishesTooLong(t *testing.T) {
	buf := captureLog(t)

	// Truncation: cause logged.
	body, err := readAnthropicStream(t, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n")
	if err == nil || !strings.Contains(buf.String(), "unexpected EOF") {
		t.Errorf("truncation not logged: err=%v log=%q body=%q", err, buf.String(), body)
	}

	// Upstream error event: type/message logged.
	buf.Reset()
	readAnthropicStream(t, "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n") //nolint:errcheck
	if !strings.Contains(buf.String(), "overloaded_error") {
		t.Errorf("error event not logged: %q", buf.String())
	}

	// Ollama translator: truncation logged too.
	buf.Reset()
	io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n")), "/api/chat", "m")) //nolint:errcheck
	if !strings.Contains(buf.String(), "unexpected EOF") {
		t.Errorf("ollama truncation not logged: %q", buf.String())
	}

	// Oversized frame gets a distinct client message.
	huge := "data: " + strings.Repeat("a", 5<<20) + "\n\n"
	raw, _ := io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader(huge)), "/api/chat", "m"))
	if !strings.Contains(string(raw), "frame too large") {
		t.Errorf("ollama: want frame-too-large message, got %.200q", raw)
	}
	body, _ = readAnthropicStream(t, huge)
	if !strings.Contains(body, "frame too large") {
		t.Errorf("anthropic: want frame-too-large message, got %.200q", body)
	}
}

func TestOllamaPath_UpstreamErrorBodiesBecomeStringShape(t *testing.T) {
	cases := map[string]string{
		"openai":    `{"error":{"message":"bad key","type":"invalid_request_error"}}`,
		"anthropic": `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`,
	}
	for name, upstream := range cases {
		inner := anthropicRTFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 401, Status: "401 Unauthorized", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstream)), ContentLength: -1}, nil
		})
		var rt http.RoundTripper = inner
		if name == "anthropic" {
			rt = &anthropicTransport{inner: inner, apiKey: "k"}
		}
		req, _ := http.NewRequest(http.MethodPost, "http://cloud.invalid/x", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		resp, err := (&translatingTransport{inner: rt, origPath: "/api/chat", clientModel: "m", clientStream: true}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 401 {
			t.Errorf("%s: status = %d, want 401 kept", name, resp.StatusCode)
		}
		if strings.TrimSpace(string(raw)) != `{"error":"bad key"}` {
			t.Errorf("%s: body = %q, want Ollama string-shaped error", name, raw)
		}

		// /v1 clients keep the raw body.
		req2, _ := http.NewRequest(http.MethodPost, "http://cloud.invalid/x", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		resp2, err := rt.RoundTrip(req2)
		if err != nil {
			t.Fatal(err)
		}
		raw2, _ := io.ReadAll(resp2.Body)
		if !strings.Contains(string(raw2), `"message":"bad key"`) {
			t.Errorf("%s: /v1 body should stay raw, got %q", name, raw2)
		}
	}
}

func TestOllamaSSE_DoneReasonDefaultsToStop(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	raw, err := io.ReadAll(translateSSEToNDJSON(io.NopCloser(strings.NewReader(src)), "/api/chat", "m"))
	if err != nil || !strings.Contains(string(raw), `"done_reason":"stop"`) {
		t.Errorf("err=%v body=%q", err, raw)
	}
}

func TestOpenAIFinishReason_LogsUnknown(t *testing.T) {
	buf := captureLog(t)
	if got := openAIFinishReason("brand_new_reason"); got != "stop" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(buf.String(), "brand_new_reason") {
		t.Errorf("unknown stop reason not logged: %q", buf.String())
	}
}
