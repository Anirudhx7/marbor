package proxy

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func drainBody(rc io.ReadCloser) string {
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestClientWantsStream(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"x"}`, true}, // absent -> Ollama defaults to streaming
		{`{"model":"x","stream":false}`, false},
		{`{"model":"x","stream":true}`, true},
		{`not json`, true}, // unparseable -> default true
	}
	for _, c := range cases {
		if got := clientWantsStream([]byte(c.body)); got != c.want {
			t.Errorf("clientWantsStream(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestTranslateJSONToSingleOllamaChat: a stream:false /api/chat fallback must
// yield ONE JSON object with the content in message and done:true, NOT NDJSON.
func TestTranslateJSONToSingleOllamaChat(t *testing.T) {
	openai := `{"choices":[{"message":{"content":"hello"}}],"usage":{"completion_tokens":5,"prompt_tokens":3}}`
	out := drainBody(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(openai)), "/api/chat", "llama3.2:3b"))
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("expected a single JSON object, got multiline: %q", out)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("not valid single JSON: %v (%s)", err, out)
	}
	if obj["done"] != true {
		t.Errorf("done = %v, want true", obj["done"])
	}
	msg, _ := obj["message"].(map[string]interface{})
	if msg == nil || msg["content"] != "hello" {
		t.Errorf("message content wrong: %v", obj["message"])
	}
	if obj["eval_count"].(float64) != 5 {
		t.Errorf("eval_count = %v, want 5", obj["eval_count"])
	}
}

func TestTranslateJSONToSingleOllamaGenerate(t *testing.T) {
	openai := `{"choices":[{"text":"hi there"}],"usage":{"completion_tokens":2,"prompt_tokens":1}}`
	out := drainBody(translateJSONToSingleOllama(io.NopCloser(strings.NewReader(openai)), "/api/generate", "m"))
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("expected a single JSON object, got multiline: %q", out)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("not valid single JSON: %v", err)
	}
	if obj["response"] != "hi there" {
		t.Errorf("response = %v, want \"hi there\"", obj["response"])
	}
	if obj["done"] != true {
		t.Errorf("done = %v, want true", obj["done"])
	}
}
