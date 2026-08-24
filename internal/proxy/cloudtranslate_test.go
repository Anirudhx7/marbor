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

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
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
	liveReq.AddCookie(&http.Cookie{Name: "marbor_session", Value: a.AdminToken()})
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

// jsonCloud returns an httptest.Server that answers with a single plain JSON
// response (not SSE) - matches OpenAI's real /v1/embeddings response shape,
// which is never streamed. If receivedBody is non-nil, the request body the
// marbor actually sent to this mock cloud is captured into it.
func jsonCloud(t *testing.T, responseBody string, receivedBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if receivedBody != nil {
			b, _ := io.ReadAll(r.Body)
			*receivedBody = b
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, responseBody)
	}))
}

// TestCloudFallbackAPIEmbeddingsYieldsOllamaShape verifies /api/embeddings
// cloud fallback: outbound request body is translated prompt->input, and the
// OpenAI-shaped response is translated to Ollama's native singular shape,
// preserving the cloud's real usage as prompt_eval_count.
func TestCloudFallbackAPIEmbeddingsYieldsOllamaShape(t *testing.T) {
	var received []byte
	cloud := jsonCloud(t, `{"data":[{"embedding":[0.1,0.2,0.3],"index":0}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`, &received)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","prompt":"hello"}`))
	req := httptest.NewRequest("POST", "/api/embeddings", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if _, hasPlural := got["embeddings"]; hasPlural {
		t.Errorf("response has plural 'embeddings' key, want singular 'embedding' only: %v", got)
	}
	emb, ok := got["embedding"].([]interface{})
	if !ok || len(emb) != 3 {
		t.Fatalf("embedding = %v, want a 3-element array", got["embedding"])
	}
	if got["prompt_eval_count"] != float64(5) {
		t.Errorf("prompt_eval_count = %v, want 5 (from cloud's real usage.total_tokens)", got["prompt_eval_count"])
	}

	// Outbound request must have been translated prompt -> input.
	var sentBody map[string]json.RawMessage
	if err := json.Unmarshal(received, &sentBody); err != nil {
		t.Fatalf("outbound body not valid JSON: %v (%q)", err, received)
	}
	if _, hasPrompt := sentBody["prompt"]; hasPrompt {
		t.Errorf("outbound request still has 'prompt' key, want renamed to 'input': %q", received)
	}
	var input string
	if err := json.Unmarshal(sentBody["input"], &input); err != nil || input != "hello" {
		t.Errorf("outbound 'input' = %v, want %q", sentBody["input"], "hello")
	}
}

// TestCloudFallbackAPIEmbedYieldsOllamaPluralShape verifies /api/embed cloud
// fallback: the outbound request body is passed through unmodified (it
// already uses "input"), and the OpenAI response's data[] entries are placed
// into the output array by their reported index, not by receive order.
func TestCloudFallbackAPIEmbedYieldsOllamaPluralShape(t *testing.T) {
	var received []byte
	// Deliberately out of order: index 1 arrives before index 0.
	cloud := jsonCloud(t, `{"data":[{"embedding":[9,9],"index":1},{"embedding":[1,1],"index":0}],"usage":{"total_tokens":8}}`, &received)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","input":["a","b"]}`))
	req := httptest.NewRequest("POST", "/api/embed", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Model           string      `json:"model"`
		Embeddings      [][]float64 `json:"embeddings"`
		PromptEvalCount int64       `json:"prompt_eval_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid Ollama /api/embed JSON: %v (%q)", err, rec.Body.String())
	}
	if len(got.Embeddings) != 2 {
		t.Fatalf("got %d embeddings, want 2", len(got.Embeddings))
	}
	if got.Embeddings[0][0] != 1 || got.Embeddings[1][0] != 9 {
		t.Errorf("embeddings = %v, want [[1,1],[9,9]] (reordered by index, not receive order)", got.Embeddings)
	}
	if got.PromptEvalCount != 8 {
		t.Errorf("prompt_eval_count = %d, want 8", got.PromptEvalCount)
	}

	// /api/embed's request body already uses "input" - marbor must not
	// apply the prompt->input rewrite here (that's /api/embeddings-only).
	// newCloudOnlyHandler's mock cloud has DefaultModel set, so the
	// pre-existing, unrelated model-field rewrite still applies - assert on
	// fields, not exact bytes.
	var sentReq map[string]json.RawMessage
	if err := json.Unmarshal(received, &sentReq); err != nil {
		t.Fatalf("outbound body not valid JSON: %v (%q)", err, received)
	}
	if _, hasPrompt := sentReq["prompt"]; hasPrompt {
		t.Errorf("outbound request has 'prompt' key, want none introduced for /api/embed: %q", received)
	}
	var sentInput []string
	if err := json.Unmarshal(sentReq["input"], &sentInput); err != nil || len(sentInput) != 2 || sentInput[0] != "a" || sentInput[1] != "b" {
		t.Errorf("outbound 'input' = %v, want [\"a\",\"b\"] unmodified", sentReq["input"])
	}
}

// TestCloudFallbackEmbedEmptyDataFallsThroughRaw verifies an empty data[]
// array does not panic and falls through to raw passthrough (no choices,
// no data - nothing this file knows how to translate).
func TestCloudFallbackEmbedEmptyDataFallsThroughRaw(t *testing.T) {
	raw := `{"data":[],"usage":{"total_tokens":0}}`
	cloud := jsonCloud(t, raw, nil)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","prompt":"hello"}`))
	req := httptest.NewRequest("POST", "/api/embeddings", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != raw {
		t.Errorf("body = %q, want raw passthrough %q", rec.Body.String(), raw)
	}
}

// TestCloudFallbackEmbedMalformedJSONFallsThroughRaw verifies an unparseable
// cloud response does not panic and falls through to raw passthrough.
func TestCloudFallbackEmbedMalformedJSONFallsThroughRaw(t *testing.T) {
	raw := `not json at all`
	cloud := jsonCloud(t, raw, nil)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","prompt":"hello"}`))
	req := httptest.NewRequest("POST", "/api/embeddings", body)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != raw {
		t.Errorf("body = %q, want raw passthrough %q", rec.Body.String(), raw)
	}
}

// TestCloudFallbackAPIEmbedDoesNotTriggerSingularBuilder and
// TestCloudFallbackAPIEmbeddingsDoesNotTriggerPluralBuilder lock down that
// the two native shapes are never collapsed into each other.
func TestCloudFallbackAPIEmbedDoesNotTriggerSingularBuilder(t *testing.T) {
	cloud := jsonCloud(t, `{"data":[{"embedding":[1,2],"index":0}],"usage":{"total_tokens":3}}`, nil)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","input":"hello"}`))
	req := httptest.NewRequest("POST", "/api/embed", body)
	h.ServeHTTP(rec, req)

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if _, hasSingular := got["embedding"]; hasSingular {
		t.Errorf("/api/embed response has singular 'embedding' key, want plural 'embeddings' only: %v", got)
	}
}

func TestCloudFallbackAPIEmbeddingsDoesNotTriggerPluralBuilder(t *testing.T) {
	cloud := jsonCloud(t, `{"data":[{"embedding":[1,2],"index":0}],"usage":{"total_tokens":3}}`, nil)
	defer cloud.Close()

	h, _ := newCloudOnlyHandler(t, cloud.URL)
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"model":"nomic-embed-text","prompt":"hello"}`))
	req := httptest.NewRequest("POST", "/api/embeddings", body)
	h.ServeHTTP(rec, req)

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if _, hasPlural := got["embeddings"]; hasPlural {
		t.Errorf("/api/embeddings response has plural 'embeddings' key, want singular 'embedding' only: %v", got)
	}
}
