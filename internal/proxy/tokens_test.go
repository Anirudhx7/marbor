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

// TestNoNewlineTailBoundedAtEmbedTailMax is a regression test for the finding
// that a no-newline response body (e.g. /v1/embeddings, whose usage
// field trails a large embedding array with no '\n' anywhere) used to grow
// the retained tail all the way to maxRequestBodyBytes (32 MiB) per request -
// a burst of concurrent embeddings requests could each pin 32 MiB, OOM-
// killing the control plane on anonymous traffic. Retention must now stop at
// embedTailMax, well below that bound, and mark the tail truncated so
// tokenCount reports -1 (unknown) instead of a fake 0 once real usage
// data past the cut is unreachable.
func TestNoNewlineTailBoundedAtEmbedTailMax(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	// A single JSON document with no newline anywhere, larger than
	// embedTailMax, simulating a large real /v1/embeddings response whose
	// "usage" field would trail a huge "embedding" array.
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = 'x'
	}
	total := 0
	for total < embedTailMax+len(chunk) {
		rec.Write(chunk)
		total += len(chunk)
	}

	if len(rec.tail) > embedTailMax {
		t.Fatalf("tail length = %d, want <= embedTailMax (%d)", len(rec.tail), embedTailMax)
	}
	if !rec.truncatedTail {
		t.Fatal("truncatedTail = false, want true after exceeding embedTailMax with no newline seen")
	}
	// A truncated no-newline body must never report a fake 0 - callers OR in
	// truncatedTail alongside aborted before calling tokenCount (proxy.go).
	if got := rec.tokenCount(true); got != -1 {
		t.Errorf("tokenCount(true) on truncated tail = %d, want -1 (unknown)", got)
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
