package proxy

import (
	"net/http/httptest"
	"testing"
	"time"
)

func recorderWith(body string) *statusRecorder {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte(body))
	return rec
}

func TestTokenCountOllamaNDJSON(t *testing.T) {
	body := `{"model":"llama3","response":"hi","done":false}
{"model":"llama3","done":true,"prompt_eval_count":26,"eval_count":290}
`
	if got := recorderWith(body).tokenCount(false); got != 316 {
		t.Errorf("tokenCount = %d, want 316 (eval_count + prompt_eval_count)", got)
	}
}

func TestTokenCountOpenAIUsage(t *testing.T) {
	body := `{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	if got := recorderWith(body).tokenCount(false); got != 30 {
		t.Errorf("tokenCount = %d, want 30 (usage.total_tokens)", got)
	}
}

func TestTokenCountSSEFraming(t *testing.T) {
	body := "data: {\"choices\":[]}\n\ndata: {\"usage\":{\"total_tokens\":42}}\n\ndata: [DONE]\n"
	if got := recorderWith(body).tokenCount(false); got != 42 {
		t.Errorf("tokenCount = %d, want 42 (final SSE chunk)", got)
	}
}

func TestTokenCountMissingReturnsZero(t *testing.T) {
	if got := recorderWith(`{"models":[]}`).tokenCount(false); got != 0 {
		t.Errorf("tokenCount = %d, want 0 when no counts present", got)
	}
	if got := recorderWith("not json at all").tokenCount(false); got != 0 {
		t.Errorf("tokenCount = %d, want 0 for non-JSON body", got)
	}
}

func TestTokenCountAbortedReturnsUnknownSentinel(t *testing.T) {
	if got := recorderWith(`{"models":[]}`).tokenCount(true); got != -1 {
		t.Errorf("tokenCount = %d, want -1 (unknown) when aborted with no final chunk", got)
	}
}

func TestTokenCountLegacyEmbeddingsReturnsUnknownSentinel(t *testing.T) {
	body := `{"embedding":[0.1,0.2,0.3]}`
	if got := recorderWith(body).tokenCount(false); got != -1 {
		t.Errorf("tokenCount = %d, want -1 (unavailable) for legacy /api/embeddings shape", got)
	}
}

func TestTTFTMeasuresFirstWrite(t *testing.T) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), start: start}
	time.Sleep(5 * time.Millisecond)
	rec.Write([]byte("first chunk"))
	firstTTFT := rec.ttft()
	if firstTTFT <= 0 {
		t.Fatalf("ttft = %v, want > 0 after first write", firstTTFT)
	}
	time.Sleep(5 * time.Millisecond)
	rec.Write([]byte("second chunk"))
	if got := rec.ttft(); got != firstTTFT {
		t.Errorf("ttft = %v after second write, want unchanged %v (only first byte counts)", got, firstTTFT)
	}
}

func TestTTFTZeroWhenNoBytesWritten(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), start: time.Now()}
	if got := rec.ttft(); got != 0 {
		t.Errorf("ttft = %v, want 0 when no byte was ever written", got)
	}
}

func TestTTFTZeroWhenStartUnset(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte("chunk"))
	if got := rec.ttft(); got != 0 {
		t.Errorf("ttft = %v, want 0 when start was never set", got)
	}
}

func TestTokenCountTailBounded(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	// Stream far more than tailMax, then the final Ollama object.
	filler := make([]byte, 4096)
	for i := range filler {
		filler[i] = 'x'
	}
	for i := 0; i < 10; i++ {
		rec.Write(filler)
	}
	rec.Write([]byte("\n{\"done\":true,\"prompt_eval_count\":5,\"eval_count\":95}\n"))

	if len(rec.tail) > tailMax {
		t.Errorf("tail length = %d, want <= %d", len(rec.tail), tailMax)
	}
	if got := rec.tokenCount(false); got != 100 {
		t.Errorf("tokenCount = %d, want 100", got)
	}
}
