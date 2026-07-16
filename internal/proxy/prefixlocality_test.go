package proxy

// Proxy-level integration tests for rolling prefix-locality recording. These
// exercise the real ServeHTTP completion site (not just the router-level
// success-gating logic in isolation) to prove: a successfully completed
// request records the node that actually served it; a 5xx, a client
// cancellation, and an upstream timeout never record anything; and a retry
// that succeeds on the second node records that second (completing) node,
// never the first node that failed.
//
// Verification is done by calling Router.PrefixLocalityLookup directly with
// the same (ctx, model, body) after ServeHTTP returns - this reads the same
// in-memory store RecordPrefixLocality writes to, without reaching into any
// unexported field.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

func prefixLocalityRoutingConfig() config.RoutingConfig {
	return config.RoutingConfig{
		Strategy:              "warm-first",
		Fallback:              "least-connections",
		PollIntervalMs:        2000,
		UpstreamTimeoutMs:     5000,
		MaxRetries:            2,
		PrefixLocalityEnabled: true,
		PrefixLocalityWeight:  10,
	}
}

func TestPrefixLocality_SuccessRecordsCompletingNode(t *testing.T) {
	dead := deadURL(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	defer good.Close()

	r := router.New(prefixLocalityRoutingConfig(), []config.NodeConfig{
		{Name: "dead", URL: dead, Runtime: "ollama"},
		{Name: "good", URL: good.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	body := `{"model":"llama3","messages":[{"role":"user","content":"hello there"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	_, node := r.PrefixLocalityLookup(context.Background(), "llama3", []byte(body))
	if node != "good" {
		t.Errorf("preferred node after success = %q, want %q (the node that actually completed, never the failed one)", node, "good")
	}
}

func TestPrefixLocality_ServerErrorNoRecord(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()

	r := router.New(prefixLocalityRoutingConfig(), []config.NodeConfig{
		{Name: "bad", URL: bad.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	body := `{"model":"llama3","messages":[{"role":"user","content":"server error case"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	_, node := r.PrefixLocalityLookup(context.Background(), "llama3", []byte(body))
	if node != "" {
		t.Errorf("preferred node after 5xx completion = %q, want \"\" (a failed response must never become a locality hint)", node)
	}
}

func TestPrefixLocality_CancellationNoRecord(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block until the test cancels the client request
	}))
	defer slow.Close()
	defer close(release)

	r := router.New(prefixLocalityRoutingConfig(), []config.NodeConfig{
		{Name: "slow", URL: slow.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	body := `{"model":"llama3","messages":[{"role":"user","content":"cancel me"}]}`
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the handler a moment to reach the upstream call, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not return after client cancellation")
	}

	_, node := r.PrefixLocalityLookup(context.Background(), "llama3", []byte(body))
	if node != "" {
		t.Errorf("preferred node after client cancellation = %q, want \"\" (a cancelled request must never record)", node)
	}
}

func TestPrefixLocality_TimeoutNoRecord(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)

	cfg := prefixLocalityRoutingConfig()
	cfg.UpstreamTimeoutMs = 100 // fire well before the node ever responds
	cfg.MaxRetries = 0

	r := router.New(cfg, []config.NodeConfig{
		{Name: "slow", URL: slow.URL, Runtime: "ollama"},
	}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "llama3")
	}

	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	body := `{"model":"llama3","messages":[{"role":"user","content":"time out on me"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < http.StatusInternalServerError {
		t.Fatalf("expected an upstream-timeout failure status (>=500), got %d", rec.Code)
	}

	_, node := r.PrefixLocalityLookup(context.Background(), "llama3", []byte(body))
	if node != "" {
		t.Errorf("preferred node after upstream timeout = %q, want \"\" (a timed-out request must never record)", node)
	}
}
