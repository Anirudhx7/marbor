package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── fmtMs ────────────────────────────────────────────────────────────────────

func TestFmtMs(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{180, "180ms"},
		{1000, "1,000ms"},
		{22340, "22,340ms"},
		{1000000, "1,000,000ms"},
	}
	for _, c := range cases {
		got := fmtMs(c.ms)
		if got != c.want {
			t.Errorf("fmtMs(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// ── roundTo1 ─────────────────────────────────────────────────────────────────

func TestRoundTo1(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{124.11, 124.1},
		{99.199, 99.2},
		{0, 0},
		{1.05, 1.1},
	}
	for _, c := range cases {
		got := roundTo1(c.in)
		if got != c.want {
			t.Errorf("roundTo1(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ── detectModel ──────────────────────────────────────────────────────────────

func TestDetectModel_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"llama3:8b"},{"id":"mistral:7b"}]}`)
	}))
	defer srv.Close()

	model, err := detectModel(srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatalf("detectModel: %v", err)
	}
	if model != "llama3:8b" {
		t.Errorf("detectModel = %q, want %q", model, "llama3:8b")
	}
}

func TestDetectModel_empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	_, err := detectModel(srv.Client(), srv.URL, "")
	if err == nil {
		t.Fatal("expected error for empty model list, got nil")
	}
}

func TestDetectModel_authHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"llama3:8b"}]}`)
	}))
	defer srv.Close()

	_, err := detectModel(srv.Client(), srv.URL, "mykey")
	if err != nil {
		t.Fatalf("detectModel: %v", err)
	}
	if gotAuth != "Bearer mykey" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer mykey")
	}
}

// ── MeasureChatTTFT ──────────────────────────────────────────────────────────────

// sseChunk builds one SSE data line from an OpenAI-compatible delta.
func sseChunk(content string, done bool) string {
	if done {
		return "data: [DONE]\n\n"
	}
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"delta": map[string]string{"content": content}},
		},
	})
	return "data: " + string(payload) + "\n\n"
}

func TestMeasureTTFT_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// First chunk with empty content (common for role deltas).
		fmt.Fprint(w, sseChunk("", false))
		// First real token.
		fmt.Fprint(w, sseChunk("Hello", false))
		fmt.Fprint(w, sseChunk("[DONE]", true))
	}))
	defer srv.Close()

	ms, err := MeasureChatTTFT(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err != nil {
		t.Fatalf("MeasureChatTTFT: %v", err)
	}
	if ms < 0 {
		t.Errorf("MeasureChatTTFT returned negative ms: %d", ms)
	}
}

func TestMeasureTTFT_authHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunk("word", false))
		fmt.Fprint(w, sseChunk("[DONE]", true))
	}))
	defer srv.Close()

	_, err := MeasureChatTTFT(context.Background(), srv.Client(), srv.URL, "llama3:8b", "testkey")
	if err != nil {
		t.Fatalf("MeasureChatTTFT: %v", err)
	}
	if gotAuth != "Bearer testkey" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer testkey")
	}
}

func TestMeasureTTFT_httpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no nodes available", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := MeasureChatTTFT(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
}

func TestMeasureTTFT_emptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Only a [DONE] with no content - simulates a model that returned nothing.
		fmt.Fprint(w, sseChunk("[DONE]", true))
	}))
	defer srv.Close()

	_, err := MeasureChatTTFT(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err == nil {
		t.Fatal("expected error for empty stream, got nil")
	}
}

// ── MeasureChatLatency ───────────────────────────────────────────────────────

func TestMeasureChatLatency_multiChunkProducesTPOT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, sseChunk("", false)) // empty role delta, not counted
		if flusher != nil {
			flusher.Flush()
		}
		for i := 0; i < 4; i++ {
			time.Sleep(10 * time.Millisecond)
			fmt.Fprint(w, sseChunk("tok", false))
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, sseChunk("[DONE]", true))
	}))
	defer srv.Close()

	sample, err := MeasureChatLatency(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err != nil {
		t.Fatalf("MeasureChatLatency: %v", err)
	}
	if sample.TTFTMs < 0 {
		t.Errorf("TTFTMs = %d, want >= 0", sample.TTFTMs)
	}
	if sample.TPOTMs == nil {
		t.Fatal("expected a non-nil TPOTMs for a 4-content-chunk stream")
	}
	// 4 content chunks, 10ms apart -> ~30ms total / 3 gaps = ~10ms/token.
	// Generous bounds to absorb scheduler jitter on a loopback test server.
	if *sample.TPOTMs <= 0 || *sample.TPOTMs > 100 {
		t.Errorf("TPOTMs = %v, want a small positive value (~10ms)", *sample.TPOTMs)
	}
}

func TestMeasureChatLatency_singleChunkYieldsNilTPOT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseChunk("word", false))
		fmt.Fprint(w, sseChunk("[DONE]", true))
	}))
	defer srv.Close()

	sample, err := MeasureChatLatency(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err != nil {
		t.Fatalf("MeasureChatLatency: %v", err)
	}
	if sample.TPOTMs != nil {
		t.Errorf("expected nil TPOTMs for a single-chunk stream, got %v", *sample.TPOTMs)
	}
}

func TestMeasureChatLatency_httpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no nodes available", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := MeasureChatLatency(context.Background(), srv.Client(), srv.URL, "llama3:8b", "")
	if err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
}

// ── printTable / JSON output (smoke tests) ────────────────────────────────────

func TestPrintTable_nocrash(t *testing.T) {
	// Confirm printTable doesn't panic on a typical result.
	r := Result{
		Model:          "llama3:8b",
		ColdMs:         22340,
		WarmMs:         180,
		ImprovementX:   124.1,
		ImprovementPct: 99.2,
	}
	// Redirect stdout to discard - we just want a no-panic guarantee.
	// (printTable writes to os.Stdout directly; integration verified manually.)
	_ = r
	printTable(r) // should not panic
}

func TestResultJSON_roundtrip(t *testing.T) {
	r := Result{
		Model:          "llama3:8b",
		ColdMs:         22340,
		WarmMs:         180,
		ImprovementX:   124.1,
		ImprovementPct: 99.2,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"model":"llama3:8b"`, `"cold_ms":22340`, `"warm_ms":180`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON missing %q in: %s", want, js)
		}
	}
}
