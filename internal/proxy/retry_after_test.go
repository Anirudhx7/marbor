package proxy

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestSetRetryAfter_CeilsAndClamps(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "1"},
		{-time.Second, "1"},
		{time.Millisecond, "1"},
		{time.Second, "1"},
		{1001 * time.Millisecond, "2"},
		{30 * time.Second, "30"},
		{59*time.Second + 100*time.Millisecond, "60"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		setRetryAfter(rec, c.d)
		if got := rec.Header().Get("Retry-After"); got != c.want {
			t.Errorf("setRetryAfter(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestModelRateLimiterRetryAfter(t *testing.T) {
	l := newModelRateLimiter()
	if _, ok := l.retryAfter("m", "n"); ok {
		t.Fatal("no window yet: want ok=false")
	}
	one := 1
	l.allow("m", "n", &one, nil)
	d, ok := l.retryAfter("m", "n")
	if !ok || d <= 0 || d > time.Minute {
		t.Fatalf("retryAfter = %v, %v; want within (0, 1m]", d, ok)
	}
	// Read-only: asking must not spend a slot.
	l2 := newModelRateLimiter()
	two := 2
	l2.allow("m", "n", &two, nil)
	for i := 0; i < 5; i++ {
		l2.retryAfter("m", "n")
	}
	if !l2.allow("m", "n", &two, nil) {
		t.Error("retryAfter consumed a request slot")
	}
	// Scoped per node.
	if _, ok := l.retryAfter("m", "other"); ok {
		t.Error("window leaked across nodes")
	}
	// A lapsed window has no honest value.
	l.mu.Lock()
	l.stats[limiterKey("m", "n")].windowStart = time.Now().Add(-2 * time.Minute)
	l.mu.Unlock()
	if _, ok := l.retryAfter("m", "n"); ok {
		t.Error("lapsed window: want ok=false")
	}
}

func TestModelRateLimiterRetryAfterRace(t *testing.T) {
	l := newModelRateLimiter()
	limit := 100
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.allow("m", "n", &limit, nil)
				l.retryAfter("m", "n")
				l.recordTokens("m", "n", 1)
			}
		}()
	}
	wg.Wait()
}

func retryAfterRig(t *testing.T, queueMs int, withCappedNode bool) *Handler {
	t.Helper()
	var nodes []config.NodeConfig
	if withCappedNode {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"done":true}`))
		}))
		t.Cleanup(up.Close)
		nodes = []config.NodeConfig{{Name: "n", URL: up.URL, Runtime: "ollama"}}
	}
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, nodes, nil)
	r.SetQueueTimeout(queueMs)
	r.SetQueueMaxDepth(4)
	st, err := store.Open(t.TempDir() + "/ra.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	one := 1
	if withCappedNode {
		for _, n := range r.Nodes() {
			seedNode(n, "m")
		}
		if err := st.SetModelConfig(store.ModelConfig{Model: "m", Node: "n", RPM: &one}); err != nil {
			t.Fatal(err)
		}
	}
	a := admin.NewServer(r, nil, config.Config{}, st)
	// Registered after the store's Close so it runs first.
	t.Cleanup(a.Shutdown)
	h := NewHandler(r, a, nil)
	if withCappedNode {
		h.modelLimiter.allow("m", "n", &one, nil)
	}
	return h
}

func postBody(h *Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","prompt":"x"}`)))
	return rec
}

func TestRetryAfter_ModelRateLimited429(t *testing.T) {
	h := retryAfterRig(t, 100, true)
	rec := postBody(h, "/api/generate")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d (%s), want 429", rec.Code, rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > 60 {
		t.Errorf("Retry-After = %q, want integer in [1,60]", rec.Header().Get("Retry-After"))
	}
}

func TestRetryAfter_NoNodesQueueTimeout(t *testing.T) {
	h := retryAfterRig(t, 100, false)
	rec := postBody(h, "/api/generate")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 (ceil of the 100ms queue timeout, clamped)", got)
	}
	// The non-Ollama-filtered variant lands on the same refusal.
	rec = postBody(h, "/v1/chat/completions")
	if rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
		t.Error("/v1 no-nodes 503 missing Retry-After")
	}
}

func TestRetryAfter_NoNodesQueueDisabledOmitted(t *testing.T) {
	h := retryAfterRig(t, 0, false)
	rec := postBody(h, "/api/generate")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want omitted when the queue is disabled", got)
	}
}

func TestRetryAfter_NoNodesCtxCancelledOmitted(t *testing.T) {
	h := retryAfterRig(t, 5000, false)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"m","prompt":"x"}`)).WithContext(ctx)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want omitted when the client went away", got)
	}
}

func TestRetryAfter_UpstreamHeaderPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"busy"}`))
	}))
	defer up.Close()
	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{{Name: "n", URL: up.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		seedNode(n, "m")
	}
	a := admin.NewServer(r, nil, config.Config{})
	t.Cleanup(a.Shutdown)
	h := NewHandler(r, a, nil)
	rec := postBody(h, "/api/generate")
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("status %d, Retry-After = %q, want the engine's 7 unchanged", rec.Code, got)
	}
}

// retryAfterExempt lists the proxy-originated 429/503 refusals that carry no
// Retry-After on purpose, keyed by the error-code expression at the call.
var retryAfterExempt = map[string]string{
	"CloudReasonLocalOnlyBlocked": "a local_only key on a cloud-only model is a permanent policy conflict; a retry can never succeed",
	"code":                        "cloud-only refusals (list unreadable or no cloud route) are permanent policy refusals; a retry can never succeed",
	`"cloud_budget_exceeded"`:     "resets on a calendar boundary that needs an admin accessor to read; no honest value here yet",
}

// TestEveryRetryableRefusalHasRetryAfter scans the non-test sources and
// requires each writeAPIError call answering 429 or 503 to be preceded by a
// Retry-After setter or to be on the exempt list above.
func TestEveryRetryableRefusalHasRetryAfter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		text := func(n ast.Node) string {
			return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "writeAPIError" || len(call.Args) < 5 {
				return true
			}
			status := text(call.Args[1])
			if status != "http.StatusTooManyRequests" && status != "http.StatusServiceUnavailable" {
				return true
			}
			seen++
			code := text(call.Args[4])
			if _, ok := retryAfterExempt[code]; ok {
				return true
			}
			if !precededByRetryAfter(string(src), fset.Position(call.Pos()).Offset) {
				t.Errorf("%s:%d: %s refusal (%s) has no Retry-After setter before it and is not exempt",
					f, fset.Position(call.Pos()).Line, status, code)
			}
			return true
		})
	}
	if seen < 8 {
		t.Errorf("scanner found only %d 429/503 writeAPIError calls; the scan is broken", seen)
	}
}

// precededByRetryAfter reports whether a Retry-After setter appears in the
// few lines just before offset.
func precededByRetryAfter(src string, offset int) bool {
	lines := strings.Split(src[:offset], "\n")
	from := max(len(lines)-8, 0)
	window := strings.Join(lines[from:], "\n")
	return strings.Contains(window, "setRetryAfter(") || strings.Contains(window, "setNoNodesRetryAfter(")
}
