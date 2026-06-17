package proxy

// cloudtranslate_test.go -- tests for the OpenAI -> Ollama NDJSON response
// translation layer used in cloud fallback paths.
//
// Each test stands up a mock cloud httptest.Server that returns OpenAI-format
// responses (SSE or JSON), routes a client request through the real Handler
// (with all local nodes marked unhealthy so cloud fallback fires), and asserts
// the client receives well-formed Ollama NDJSON.

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

// newCloudOnlyHandler builds a Handler whose single node is down, so every
// request falls back to the supplied cloud server.
func newCloudOnlyHandler(t *testing.T, cloudURL string) (*Handler, *admin.Server) {
	t.Helper()
	cloud := config.CloudProvider{
		Name:            "test-cloud",
		Provider:        "openai",
		BaseURL:         cloudURL,
		APIKey:          "test-key",
		DefaultModel:    "gpt-4o",
		CostPer1KTokens: 0.002,
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

// sseCloud returns an httptest.Server that emits a two-chunk OpenAI SSE
// stream followed by a usage chunk and [DONE].
func sseCloud(t *testing.T, chunks []string, promptTokens, completionTokens int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock cloud ResponseWriter does not implement http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			io.WriteString(w, "data: "+c+"\n\n")
			f.Flush()
		}
		// Final usage chunk (mirrors OpenAI streaming behavior).
		usage, _ := json.Marshal(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []interface{}{},
			"usage": map[string]int64{
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
				"total_tokens":      promptTokens + completionTokens,
			},
		})
		io.WriteString(w, "data: "+string(usage)+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
}

// parseNDJSON reads all NDJSON lines from body into a slice of raw JSON maps.
func parseNDJSON(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var lines []map[string]interface{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("NDJSON line is not valid JSON: %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// TestCloudFallbackAPIChatYieldsOllamaNDJSON sends a /api/chat request that
// falls back to a cloud SSE provider and verifies the client receives Ollama
// NDJSON (not raw OpenAI SSE).
func TestCloudFallbackAPIChatYieldsOllamaNDJSON(t *testing.T) {
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"Hel"}}]}`,
		`{"id":"c1","choices":[{"delta":{"content":"lo"}}]}`,
	}
	cloud := sseCloud(t, chunks, 10, 20)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"llama3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req := httptest.NewRequest("POST", "/api/chat", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Must NOT be raw OpenAI SSE.
	respBody := rec.Body.String()
	if strings.Contains(respBody, "data: ") {
		t.Errorf("client received raw SSE (not translated): %q", respBody)
	}
	if strings.Contains(respBody, "[DONE]") {
		t.Errorf("client received SSE [DONE] sentinel (not translated): %q", respBody)
	}

	lines := parseNDJSON(t, respBody)
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want >= 2 (content lines + final done:true)", len(lines))
	}

	// Content lines must carry message.content and done:false.
	for i, l := range lines[:len(lines)-1] {
		if l["done"] != false {
			t.Errorf("line %d: done = %v, want false", i, l["done"])
		}
		msg, ok := l["message"].(map[string]interface{})
		if !ok {
			t.Errorf("line %d: no 'message' object: %v", i, l)
			continue
		}
		if msg["role"] != "assistant" {
			t.Errorf("line %d: role = %v, want assistant", i, msg["role"])
		}
	}

	// Final line must be done:true.
	last := lines[len(lines)-1]
	if last["done"] != true {
		t.Errorf("last line done = %v, want true", last["done"])
	}

	// Model echoed back must be the original Ollama model, not gpt-4o.
	for i, l := range lines {
		if l["model"] != "llama3" {
			t.Errorf("line %d: model = %v, want llama3 (original client model)", i, l["model"])
		}
	}
}

// TestCloudFallbackAPIGenerateYieldsOllamaNDJSON verifies /api/generate cloud
// fallback emits {"response":...} NDJSON (not {"message":...}).
func TestCloudFallbackAPIGenerateYieldsOllamaNDJSON(t *testing.T) {
	chunks := []string{
		`{"id":"g1","choices":[{"delta":{"content":"World"}}]}`,
	}
	cloud := sseCloud(t, chunks, 5, 10)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"codellama","prompt":"hello","stream":true}`))
	req := httptest.NewRequest("POST", "/api/generate", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	lines := parseNDJSON(t, rec.Body.String())
	if len(lines) < 2 {
		t.Fatalf("got %d NDJSON lines, want >= 2", len(lines))
	}

	// Content lines must carry "response" key, not "message".
	for i, l := range lines[:len(lines)-1] {
		if _, hasMsg := l["message"]; hasMsg {
			t.Errorf("line %d: has 'message' key, expected 'response' for /api/generate", i)
		}
		if _, hasResp := l["response"]; !hasResp {
			t.Errorf("line %d: missing 'response' key: %v", i, l)
		}
		if l["done"] != false {
			t.Errorf("line %d: done = %v, want false", i, l["done"])
		}
	}

	last := lines[len(lines)-1]
	if last["done"] != true {
		t.Errorf("last line done = %v, want true", last["done"])
	}

	for i, l := range lines {
		if l["model"] != "codellama" {
			t.Errorf("line %d: model = %v, want codellama", i, l["model"])
		}
	}
}

// TestCloudFallbackV1PathPassthrough verifies that /v1/chat/completions
// requests that fall back to cloud receive raw OpenAI SSE unchanged (no
// translation).
func TestCloudFallbackV1PathPassthrough(t *testing.T) {
	rawEvents := []string{
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range rawEvents {
			io.WriteString(w, ev)
			if f != nil {
				f.Flush()
			}
		}
	}))
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"gpt-4o","messages":[],"stream":true}`))
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Response must be raw SSE (still contains "data: " prefix and [DONE]).
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "data: ") {
		t.Errorf("/v1/ path: expected raw SSE passthrough but got: %q", respBody)
	}
	if !strings.Contains(respBody, "[DONE]") {
		t.Errorf("/v1/ path: expected [DONE] in passthrough response but got: %q", respBody)
	}
}

// TestCloudFallbackTokenCountNonZero verifies that the admin token counter is
// updated when the cloud returns usage data and the path is /api/chat.
// The translated NDJSON final line carries eval_count + prompt_eval_count
// which statusRecorder.tokenCount() picks up and passes to admin.LogRequest.
func TestCloudFallbackTokenCountNonZero(t *testing.T) {
	chunks := []string{
		`{"id":"c1","choices":[{"delta":{"content":"answer"}}]}`,
	}
	cloud := sseCloud(t, chunks, 7, 35) // 42 total
	defer cloud.Close()

	h, a := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`))
	req := httptest.NewRequest("POST", "/api/chat", body)
	h.ServeHTTP(rec, req)

	// Read token count from the live requests log (Tokens field on RequestLog).
	reqRec := httptest.NewRecorder()
	liveReq := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	liveReq.Header.Set("Authorization", "Bearer "+a.AdminToken())
	a.Handler().ServeHTTP(reqRec, liveReq)
	if reqRec.Code != http.StatusOK {
		t.Fatalf("live requests status = %d", reqRec.Code)
	}
	var entries []struct {
		Tokens int64 `json:"tokens"`
	}
	if err := json.NewDecoder(reqRec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode live requests: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d request log entries, want 1", len(entries))
	}
	if entries[0].Tokens == 0 {
		t.Errorf("logged Tokens = 0, want non-zero (cloud reported usage 7+35=42)")
	}
}
