package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// newShowTestRouter returns a bare Router with a real *http.Client, enough
// to exercise FetchModelShow without needing a full node list.
func newShowTestRouter() *Router {
	return New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 60000}, nil, nil)
}

// TestFetchModelShow_Success verifies the happy path: a complete model_info
// block (all five facts the KV-cache formula needs) decodes into
// ModelShowInfo with ok=true.
func TestFetchModelShow_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Name != "llama3.1:8b" {
			t.Errorf("request name = %q, want llama3.1:8b", body.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model_info": map[string]interface{}{
				"general.architecture":          "llama",
				"llama.context_length":          131072,
				"llama.block_count":             32,
				"llama.attention.head_count":    32,
				"llama.attention.head_count_kv": 8,
				"llama.embedding_length":        4096,
			},
		})
	}))
	defer srv.Close()

	r := newShowTestRouter()
	info, ok := r.FetchModelShow(srv.URL, "llama3.1:8b")
	if !ok {
		t.Fatal("expected ok=true for a complete model_info block")
	}
	if info.ContextLength != 131072 {
		t.Errorf("ContextLength = %d, want 131072", info.ContextLength)
	}
	if info.BlockCount != 32 {
		t.Errorf("BlockCount = %d, want 32", info.BlockCount)
	}
	if info.HeadCount != 32 {
		t.Errorf("HeadCount = %d, want 32", info.HeadCount)
	}
	if info.HeadCountKV != 8 {
		t.Errorf("HeadCountKV = %d, want 8", info.HeadCountKV)
	}
	if info.EmbeddingLength != 4096 {
		t.Errorf("EmbeddingLength = %d, want 4096", info.EmbeddingLength)
	}
}

// TestFetchModelShow_IncompleteModelInfo verifies that a partial model_info
// block (missing a field the formula needs) returns ok=false rather than a
// zero-filled struct that could be mistaken for real data.
func TestFetchModelShow_IncompleteModelInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model_info": map[string]interface{}{
				"general.architecture": "llama",
				"llama.context_length": 8192,
				// block_count, head_count, head_count_kv, embedding_length missing
			},
		})
	}))
	defer srv.Close()

	r := newShowTestRouter()
	_, ok := r.FetchModelShow(srv.URL, "some-model")
	if ok {
		t.Error("expected ok=false for an incomplete model_info block")
	}
}

// TestFetchModelShow_MissingArchitecture verifies that a response with no
// general.architecture key (so the <arch>.* key prefix can't be built) fails
// closed rather than guessing a prefix.
func TestFetchModelShow_MissingArchitecture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model_info": map[string]interface{}{
				"context_length": 8192,
			},
		})
	}))
	defer srv.Close()

	r := newShowTestRouter()
	_, ok := r.FetchModelShow(srv.URL, "some-model")
	if ok {
		t.Error("expected ok=false when general.architecture is absent")
	}
}

// TestFetchModelShow_NodeUnreachable verifies a network error (node down)
// returns ok=false, never an error the caller must special-case - the whole
// point is that callers can treat any failure identically and fall back to
// the Estimated formula.
func TestFetchModelShow_NodeUnreachable(t *testing.T) {
	r := newShowTestRouter()
	_, ok := r.FetchModelShow("http://127.0.0.1:1", "some-model")
	if ok {
		t.Error("expected ok=false for an unreachable node")
	}
}

// TestFetchModelShow_HeadDimTruncationGuard verifies a model_info block
// where embedding_length < attention.head_count (which would make head_dim
// truncate to 0 via integer division downstream, silently zeroing the
// entire KV-cache term while still labeled "derived") is rejected as
// ok=false rather than accepted with a fabricated zero-cost answer.
func TestFetchModelShow_HeadDimTruncationGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model_info": map[string]interface{}{
				"general.architecture":          "llama",
				"llama.context_length":          8192,
				"llama.block_count":             32,
				"llama.attention.head_count":    128,
				"llama.attention.head_count_kv": 8,
				"llama.embedding_length":        64, // < head_count -> head_dim would truncate to 0
			},
		})
	}))
	defer srv.Close()

	r := newShowTestRouter()
	_, ok := r.FetchModelShow(srv.URL, "some-model")
	if ok {
		t.Error("expected ok=false when embedding_length < attention.head_count (head_dim would truncate to 0)")
	}
}

// TestFetchModelShow_NonOKStatus verifies a 404 (e.g. model not found/not
// downloaded) returns ok=false rather than attempting to decode an error body.
func TestFetchModelShow_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	r := newShowTestRouter()
	_, ok := r.FetchModelShow(srv.URL, "nonexistent-model")
	if ok {
		t.Error("expected ok=false for a non-200 response")
	}
}
