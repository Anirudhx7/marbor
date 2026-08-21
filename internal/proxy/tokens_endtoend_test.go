package proxy

// tokens_endtoend_test.go -- P80 regression coverage: two token-accounting
// bugs found during P78 Phase 2 real-embedding verification.
//
// Bug 1 (parsing): statusRecorder.tail used to cap at tailMax (8KB)
// unconditionally. A real embeddings response is a single JSON document
// (no newlines) with its "usage"/eval_count field mixed in with a large
// embedding array; once truncated, the retained fragment never starts with
// '{' and tokenCount() silently returned 0 instead of the real count.
//
// Bug 2 (logging): even when tokenCount() correctly returned -1 (unknown),
// the call site clamped it to 0 before LogRequest, so "unavailable" could
// never reach the request log/dashboard, defeating the -1 sentinel.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
)

// liveRequestTokens fetches /admin/requests/live and returns the Tokens
// field of each entry, in the same order the admin API returns them.
func liveRequestTokens(t *testing.T, a *admin.Server) []int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.AddCookie(&http.Cookie{Name: "mesh_session", Value: a.AdminToken()})
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live requests status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var entries []struct {
		Tokens int64 `json:"tokens"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode live requests: %v", err)
	}
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.Tokens
	}
	return out
}

// TestTokenCountLargeEmbeddingResponseExceedsOldTailBuffer is a unit-level
// regression test for Bug 1: a single-JSON-document embeddings response
// larger than the old 8KB tail cap, with "usage.total_tokens" present but
// positioned before a large embedding array that pushes the document well
// past 8KB. Against the pre-fix implementation (tail always capped at
// tailMax, truncating from the front) this fails because the retained
// fragment starts mid-array, not at '{', so tokenCount() falls through to 0.
func TestTokenCountLargeEmbeddingResponseExceedsOldTailBuffer(t *testing.T) {
	// A single embedding vector of 2000 floats, formatted verbosely, comfortably
	// exceeds 8KB on its own - real nomic-embed-text (768 dims) responses hit
	// the same boundary in production (see req-27ec9ce2 et al. in P80 filing).
	floats := make([]string, 2000)
	for i := range floats {
		floats[i] = "0.123456789"
	}
	body := fmt.Sprintf(
		`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[%s]}],"model":"nomic-embed-text","usage":{"prompt_tokens":9,"total_tokens":9}}`,
		strings.Join(floats, ","),
	)
	if len(body) <= tailMax {
		t.Fatalf("test body = %d bytes, want > tailMax (%d) to actually exercise the truncation boundary", len(body), tailMax)
	}

	rec := recorderWith(body)
	if got := rec.tokenCount(false); got != 9 {
		t.Errorf("tokenCount = %d, want 9 (usage.total_tokens) for a %d-byte single-JSON embeddings response", got, len(body))
	}
}

// TestEmbeddingsLargeResponseTokenCountLoggedEndToEnd drives the same
// oversized embeddings response through the full proxy handler and checks
// the mesh's own request_log (via /admin/requests/live), not just the
// tokenCount() unit. This is the exact path P80 was filed against: the
// backend genuinely reports a token count, but the mesh recorded tokens:0.
func TestEmbeddingsLargeResponseTokenCountLoggedEndToEnd(t *testing.T) {
	floats := make([]string, 2000)
	for i := range floats {
		floats[i] = "0.123456789"
	}
	body := fmt.Sprintf(
		`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[%s]}],"model":"nomic-embed-text","usage":{"prompt_tokens":9,"total_tokens":9}}`,
		strings.Join(floats, ","),
	)
	if len(body) <= tailMax {
		t.Fatalf("test body = %d bytes, want > tailMax (%d)", len(body), tailMax)
	}

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"hello"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	tokens := liveRequestTokens(t, a)
	if len(tokens) != 1 {
		t.Fatalf("got %d request-log entries, want 1", len(tokens))
	}
	if tokens[0] != 9 {
		t.Errorf("request_log tokens = %d, want 9 (real backend usage.total_tokens, not the pre-fix silent 0)", tokens[0])
	}
}

// TestUnavailableTokenCountPreservedThroughRequestLog is a regression test
// for Bug 2: even once tokenCount() correctly returns -1 (Ollama's legacy
// /api/embeddings response shape, which genuinely reports no token count),
// the call site at the old proxy.go:657-661 clamped negative values to 0
// before calling LogRequest, so "unavailable" could never survive into the
// request log. This drives a real request through the full handler and
// checks -1 remains -1 in /admin/requests/live, distinct from a genuine 0.
func TestUnavailableTokenCountPreservedThroughRequestLog(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"embedding":[0.1,0.2,0.3]}`))
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/embeddings", strings.NewReader(`{"model":"mock-node","prompt":"hello"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	tokens := liveRequestTokens(t, a)
	if len(tokens) != 1 {
		t.Fatalf("got %d request-log entries, want 1", len(tokens))
	}
	if tokens[0] != -1 {
		t.Errorf("request_log tokens = %d, want -1 (unavailable, must not be clamped to a fake 0)", tokens[0])
	}
}
