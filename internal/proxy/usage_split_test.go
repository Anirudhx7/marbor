package proxy

// Every body in this file is synthetic: hand-written in the documented
// OpenAI-compatible shape, not captured from a running engine.

import (
	"bytes"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func ptrTo(n int64) *int64 { return &n }

func eqPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestUsageSplit(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		aborted    bool
		wantInput  *int64
		wantCached *int64
		wantTotal  int64
	}{
		{
			name:       "synthetic vllm non-stream details cached>0",
			body:       `{"id":"c1","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":20,"total_tokens":2020,"prompt_tokens_details":{"cached_tokens":1820}}}`,
			wantInput:  ptrTo(2000),
			wantCached: ptrTo(1820),
			wantTotal:  2020,
		},
		{
			name:       "synthetic real zero cached is kept",
			body:       `{"usage":{"prompt_tokens":1450,"completion_tokens":5,"total_tokens":1455,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantInput:  ptrTo(1450),
			wantCached: ptrTo(0),
			wantTotal:  1455,
		},
		{
			name:      "synthetic details null (vllm flag off)",
			body:      `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":null}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic details object without cached_tokens",
			body:      `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"audio_tokens":0}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic no usage at all",
			body:      `{"models":[]}`,
			wantTotal: 0,
		},
		{
			name:       "synthetic llama.cpp non-stream",
			body:       `{"choices":[],"usage":{"completion_tokens":8,"prompt_tokens":120,"total_tokens":128,"prompt_tokens_details":{"cached_tokens":100}},"timings":{"cache_n":100}}`,
			wantInput:  ptrTo(120),
			wantCached: ptrTo(100),
			wantTotal:  128,
		},
		{
			name:       "synthetic sse usage chunk then done",
			body:       "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":2,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: [DONE]\n\n",
			wantInput:  ptrTo(50),
			wantCached: ptrTo(32),
			wantTotal:  52,
		},
		{
			name:      "synthetic sse without usage",
			body:      "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantTotal: 0,
		},
		{
			name:       "synthetic sse last json line is a content chunk, earlier line has usage",
			body:       "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n",
			wantInput:  ptrTo(50),
			wantCached: ptrTo(32),
			wantTotal:  52,
		},
		{
			name:       "synthetic usage null chunk before the real one",
			body:       "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: {\"choices\":[],\"usage\":null}\n\ndata: [DONE]\n\n",
			wantInput:  ptrTo(50),
			wantCached: ptrTo(32),
			wantTotal:  52,
		},
		{
			name:      "synthetic ollama ndjson",
			body:      "{\"response\":\"hi\",\"done\":false}\n{\"done\":true,\"prompt_eval_count\":26,\"eval_count\":290}\n",
			wantTotal: 316,
		},
		{
			name:      "synthetic ollama v1 without details",
			body:      `{"usage":{"prompt_tokens":26,"completion_tokens":290,"total_tokens":316}}`,
			wantTotal: 316,
		},
		{
			name:       "synthetic multi-line pretty printed json",
			body:       "{\n  \"usage\": {\n    \"prompt_tokens\": 100,\n    \"total_tokens\": 110,\n    \"prompt_tokens_details\": {\n      \"cached_tokens\": 64\n    }\n  }\n}\n",
			wantInput:  ptrTo(100),
			wantCached: ptrTo(64),
			wantTotal:  110,
		},
		{
			name:      "synthetic aborted stream",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\n",
			aborted:   true,
			wantTotal: 52,
		},
		{
			name:      "synthetic unparseable last json line",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: {\"choices\":[{\"del",
			wantTotal: -1,
		},
		{
			name:      "synthetic legacy embedding",
			body:      `{"embedding":[0.1,0.2]}`,
			wantTotal: -1,
		},
		{
			name:      "synthetic cached greater than prompt",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":11}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic prompt_tokens zero",
			body:      `{"usage":{"prompt_tokens":0,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic negative cached",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":-3}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic float cached does not break the total",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":1.5}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic string cached does not break the total",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":"7"}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic details as array does not break the total",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":[1,2]}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic details as string does not break the total",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":"none"}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic string prompt_tokens does not break the total",
			body:      `{"usage":{"prompt_tokens":"10","total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic cached_tokens null",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":null}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic prompt_tokens float with zero fraction",
			body:      `{"usage":{"prompt_tokens":10.0,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic usage as string",
			body:      `{"usage":"none"}`,
			wantTotal: -1,
		},
		{
			name:      "synthetic usage as array",
			body:      `{"usage":[1,2]}`,
			wantTotal: -1,
		},
		{
			name:      "synthetic cached_tokens overflows int64",
			body:      `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":99999999999999999999}}}`,
			wantTotal: 15,
		},
		{
			name:      "synthetic prompt_tokens above the per-request cap",
			body:      `{"usage":{"prompt_tokens":9223372036854775807,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":9223372036854775807}}}`,
			wantTotal: 15,
		},
		{
			name:       "synthetic cached equals prompt",
			body:       `{"usage":{"prompt_tokens":10,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":10}}}`,
			wantInput:  ptrTo(10),
			wantCached: ptrTo(10),
			wantTotal:  15,
		},
		{
			name:      "synthetic sse wrong-typed cached",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":\"32\"}}}\n\ndata: [DONE]\n\n",
			wantTotal: 52,
		},
		{
			name:      "synthetic sse prompt_tokens without details",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52}}\n\ndata: [DONE]\n\n",
			wantTotal: 52,
		},
		{
			// usageSplit stops at the last line that carries a usage object,
			// even one tokenCount skips for a zero total: the later zero-total
			// object is what the split describes, so it is unknown.
			name:      "synthetic sse later zero-total usage object wins over earlier usage",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: {\"usage\":{\"prompt_tokens\":0,\"total_tokens\":0}}\n\ndata: [DONE]\n\n",
			wantTotal: 52,
		},
		{
			name:      "synthetic sse wrong-typed usage line is the last json line",
			body:      "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: {\"usage\":5}\n\ndata: [DONE]\n\n",
			wantTotal: -1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := recorderWith(c.body)
			got := rec.usageSplit(c.aborted)
			if !eqPtr(got.Input, c.wantInput) || !eqPtr(got.Cached, c.wantCached) {
				t.Errorf("usageSplit = input %v cached %v, want input %v cached %v", deref(got.Input), deref(got.Cached), deref(c.wantInput), deref(c.wantCached))
			}
			if n := rec.tokenCount(c.aborted); n != c.wantTotal {
				t.Errorf("tokenCount = %d, want %d (must not change)", n, c.wantTotal)
			}
		})
	}
}

func deref(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

func TestUsageSplitOversizeTailIsUnknown(t *testing.T) {
	huge := `data: {"pad":"` + strings.Repeat("u", embedTailMax+10) + `","usage":{"prompt_tokens":5,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":2}}}` + "\n\n"
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte(huge))
	rec.Write([]byte("data: [DONE]\n\n"))
	if !rec.truncatedTail {
		t.Fatalf("truncatedTail = false, want true for a line larger than the retention cap")
	}
	got := rec.usageSplit(rec.truncatedTail)
	if got.Input != nil || got.Cached != nil {
		t.Errorf("usageSplit = %v/%v, want nil for a usage line too large to retain", deref(got.Input), deref(got.Cached))
	}
}

func TestUsageSplitLargeSingleDocBeyondTailMax(t *testing.T) {
	floats := strings.Repeat("0.123456789,", 2000)
	body := `{"data":[{"embedding":[` + floats + `0.1]}],"usage":{"prompt_tokens":9,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":3}}}`
	if len(body) <= tailMax {
		t.Fatalf("body too small to exercise the boundary")
	}
	got := recorderWith(body).usageSplit(false)
	if got.Input == nil || *got.Input != 9 || got.Cached == nil || *got.Cached != 3 {
		t.Errorf("usageSplit = %v/%v, want 9/3", deref(got.Input), deref(got.Cached))
	}
}

// A usage block with unusable prompt_tokens_details is logged once per
// process, not once per request.
func TestMalformedSplitLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	malformedSplitLogged.Store(false)
	bad := []byte(`{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":"x"}}`)
	for i := 0; i < 5; i++ {
		if got := splitFromUsage(bad); got.Input != nil || got.Cached != nil {
			t.Fatalf("malformed usage produced a split: %v/%v", deref(got.Input), deref(got.Cached))
		}
	}
	if n := strings.Count(buf.String(), "prompt_tokens_details"); n != 1 {
		t.Errorf("malformed-split log lines = %d, want exactly 1:\n%s", n, buf.String())
	}
	buf.Reset()
	splitFromUsage([]byte(`{"prompt_tokens":10}`))
	splitFromUsage([]byte(`{"prompt_tokens":10,"prompt_tokens_details":null}`))
	if buf.Len() != 0 {
		t.Errorf("a runtime that simply does not report a split must not log, got %q", buf.String())
	}
}
