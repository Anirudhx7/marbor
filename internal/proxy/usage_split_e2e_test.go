package proxy

// Backend bodies here are synthetic: written to the documented
// OpenAI-compatible and Ollama shapes, not captured from a running engine.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
)

type liveUsage struct {
	Tokens       int64  `json:"tokens"`
	CachedTokens *int64 `json:"cachedTokens"`
	InputTokens  *int64 `json:"inputTokens"`
}

func liveUsages(t *testing.T, a *admin.Server) []liveUsage {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
	req.AddCookie(&http.Cookie{Name: "marbor_session", Value: a.AdminToken()})
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live requests status = %d", rec.Code)
	}
	var out []liveUsage
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// waitLiveUsages polls the live list until it holds one entry or 5s pass;
// logging runs after the response finishes, so the entry can lag the client.
func waitLiveUsages(t *testing.T, a *admin.Server) []liveUsage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []liveUsage
	for time.Now().Before(deadline) {
		if got = liveUsages(t, a); len(got) == 1 {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got
}

const syntheticUsageJSON = `{"id":"x","choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":20,"total_tokens":2020,"prompt_tokens_details":{"cached_tokens":1820}}}`

// Each client-facing endpoint shape is logged through the same path; the
// OpenAI-shaped ones report a split, the Ollama-native ones never do.
func TestUsageSplitLoggedPerEndpointShape(t *testing.T) {
	cases := []struct {
		name, path, reqBody, respBody string
		wantSplit                     bool
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"mock-node","messages":[{"role":"user","content":"hi"}]}`, syntheticUsageJSON, true},
		{"completions", "/v1/completions", `{"model":"mock-node","prompt":"hi"}`, syntheticUsageJSON, true},
		{"embeddings", "/v1/embeddings", `{"model":"mock-node","input":"hi"}`, syntheticUsageJSON, true},
		{"ollama v1 chat without details", "/v1/chat/completions", `{"model":"mock-node","messages":[{"role":"user","content":"hi"}]}`, `{"usage":{"prompt_tokens":26,"completion_tokens":290,"total_tokens":316}}`, false},
		{"ollama chat", "/api/chat", `{"model":"mock-node","messages":[{"role":"user","content":"hi"}]}`, `{"done":true,"prompt_eval_count":26,"eval_count":290}`, false},
		{"ollama generate", "/api/generate", `{"model":"mock-node","prompt":"hi"}`, `{"done":true,"prompt_eval_count":26,"eval_count":290}`, false},
		{"ollama embed", "/api/embed", `{"model":"mock-node","input":"hi"}`, `{"embeddings":[[0.1]],"prompt_eval_count":4}`, false},
		{"ollama embeddings", "/api/embeddings", `{"model":"mock-node","prompt":"hi"}`, `{"embedding":[0.1,0.2]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(c.respBody))
			}))
			defer node.Close()
			h, a := newStreamTestHandler(t, node.URL)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.reqBody)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			got := liveUsages(t, a)
			if len(got) != 1 {
				t.Fatalf("entries = %d, want 1", len(got))
			}
			if c.wantSplit {
				if got[0].CachedTokens == nil || *got[0].CachedTokens != 1820 || got[0].InputTokens == nil || *got[0].InputTokens != 2000 {
					t.Errorf("split = %v/%v, want 1820/2000", got[0].CachedTokens, got[0].InputTokens)
				}
			} else if got[0].CachedTokens != nil || got[0].InputTokens != nil {
				t.Errorf("split = %v/%v, want nil (runtime does not report it)", got[0].CachedTokens, got[0].InputTokens)
			}
		})
	}
}

// A streamed reply is relayed as it arrives and the final usage chunk is still
// read from the retained tail: the first chunk reaches the client while the
// backend is still holding the rest.
func TestUsageSplitStreamStaysStreamingAndIsLogged(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		f.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":2,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\ndata: [DONE]\n\n")
		f.Flush()
	}))
	defer node.Close()
	h, a := newStreamTestHandler(t, node.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"mock-node","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	done := make(chan struct{})
	go func() {
		resp.Body.Read(buf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk was not relayed while the backend still held the rest: response is being buffered")
	}
	closeRelease()
	io.Copy(io.Discard, resp.Body)

	got := waitLiveUsages(t, a)
	if len(got) != 1 || got[0].CachedTokens == nil || *got[0].CachedTokens != 32 || *got[0].InputTokens != 50 {
		t.Errorf("logged = %+v, want cached 32 input 50", got)
	}
}

// A gzip-compressed reply cannot be read from the tail, so it logs no split
// (unknown), and the client still receives the intact compressed bytes.
func TestUsageSplitGzipResponseIsUnknown(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(syntheticUsageJSON))
	zw.Close()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
	}))
	defer node.Close()
	h, a := newStreamTestHandler(t, node.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"mock-node","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip passed through (the proxy must not change the body)", resp.Header.Get("Content-Encoding"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, gz.Bytes()) {
		t.Errorf("client received %d bytes, want the %d compressed bytes untouched", len(body), gz.Len())
	}
	got := waitLiveUsages(t, a)
	if len(got) != 1 || got[0].CachedTokens != nil || got[0].InputTokens != nil {
		t.Errorf("logged = %+v, want one entry with no split for a compressed body", got)
	}
}

// An upstream error response is not a measurement: even if its body carries a
// usage block, its row has no split.
func TestUsageSplitErrorResponseLoggedWithoutSplit(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"boom"},"usage":{"prompt_tokens":50,"total_tokens":52,"prompt_tokens_details":{"cached_tokens":32}}}`))
	}))
	defer node.Close()
	h, a := newStreamTestHandler(t, node.URL)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mock-node","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 relayed", rec.Code)
	}
	got := waitLiveUsages(t, a)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].CachedTokens != nil || got[0].InputTokens != nil {
		t.Errorf("split = %v/%v, want nil for an error response", got[0].CachedTokens, got[0].InputTokens)
	}
}

// A client that goes away mid-stream is an aborted request: even when a usage
// chunk was already sent, it logs no split.
func TestUsageSplitAbortedStreamLoggedWithoutSplit(t *testing.T) {
	started := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":50,\"total_tokens\":52,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\n")
		f.Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer node.Close()
	h, a := newStreamTestHandler(t, node.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"mock-node","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	resp.Body.Read(buf)
	<-started
	cancel()
	resp.Body.Close()

	got := waitLiveUsages(t, a)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].CachedTokens != nil || got[0].InputTokens != nil {
		t.Errorf("split = %v/%v, want nil for an aborted request", got[0].CachedTokens, got[0].InputTokens)
	}
}
