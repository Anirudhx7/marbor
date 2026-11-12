package proxy

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
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
		{60 * time.Second, "60"},
		{time.Duration(math.MaxInt64), "9223372037"},
	}
	for _, c := range cases {
		t.Run(c.d.String(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			setRetryAfter(rec, c.d)
			if got := rec.Header().Get("Retry-After"); got != c.want {
				t.Errorf("setRetryAfter(%v) = %q, want %q", c.d, got, c.want)
			}
		})
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
	if !ok || d <= 59*time.Second || d > time.Minute {
		t.Fatalf("retryAfter = %v, %v; want within (59s, 1m] for a fresh window", d, ok)
	}
	// A window that started 30s ago has about 30s left.
	l.mu.Lock()
	l.stats[limiterKey("m", "n")].windowStart = time.Now().Add(-30 * time.Second)
	l.mu.Unlock()
	if d, ok := l.retryAfter("m", "n"); !ok || d < 29*time.Second || d > 30*time.Second {
		t.Fatalf("retryAfter = %v, %v; want 29s-30s for a window started 30s ago", d, ok)
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

// Has no assertions of its own: it is meaningful under -race, where it proves
// retryAfter shares the limiter lock with allow and recordTokens.
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
	st, err := store.Open(filepath.Join(t.TempDir(), "ra.db"))
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
	assertAPIErrorBody(t, rec, "model_rate_limited")
}

// assertAPIErrorBody checks the response is still the JSON error envelope
// with the given code.
func assertAPIErrorBody(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var b apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Error.Code != code {
		t.Errorf("body = %s (err %v), want error.code %q", rec.Body.String(), err, code)
	}
}

// The 429 carries the remaining window: one that started 30s ago yields 30.
func TestRetryAfter_ModelRateLimited429UsesWindow(t *testing.T) {
	h := retryAfterRig(t, 100, true)
	h.modelLimiter.mu.Lock()
	h.modelLimiter.stats[limiterKey("m", "n")].windowStart = time.Now().Add(-30 * time.Second)
	h.modelLimiter.mu.Unlock()
	rec := postBody(h, "/api/generate")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" && got != "29" {
		t.Errorf("Retry-After = %q, want 29 or 30", got)
	}
}

func TestRetryAfter_NoNodesQueueTimeout(t *testing.T) {
	h := retryAfterRig(t, 1500, false)
	rec := postBody(h, "/api/generate")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (the 1500ms queue timeout rounded up)", got)
	}
	assertAPIErrorBody(t, rec, "no_nodes_available")
	// The non-Ollama-filtered variant lands on the same refusal.
	rec = postBody(h, "/v1/chat/completions")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/v1 status %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("/v1 Retry-After = %q, want 2", got)
	}
}

// Every wait outcome decides whether the no-nodes refusal carries the header.
func TestSetNoNodesRetryAfter_ByOutcome(t *testing.T) {
	h := retryAfterRig(t, 2500, false)
	cases := []struct {
		outcome string
		want    string
	}{
		{router.WaitQueueTimeout, "3"},
		{router.WaitQueueFull, "3"},
		{router.WaitSLAExpired, "3"},
		{router.WaitRouted, ""},
		{router.WaitSkipHard, ""},
		{router.WaitQueueDisabled, ""},
		{router.WaitCtxCancelled, ""},
	}
	for _, c := range cases {
		t.Run(c.outcome, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.setNoNodesRetryAfter(rec, router.WaitResult{Outcome: c.outcome})
			if got := rec.Header().Get("Retry-After"); got != c.want {
				t.Errorf("Retry-After = %q, want %q", got, c.want)
			}
		})
	}
	h.router.SetQueueTimeout(0)
	rec := httptest.NewRecorder()
	h.setNoNodesRetryAfter(rec, router.WaitResult{Outcome: router.WaitQueueTimeout})
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("zero queue timeout: Retry-After = %q, want omitted", got)
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
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want the engine 429", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `{"error":"busy"}`) {
		t.Errorf("body = %q, want the engine body", got)
	}
	if got := rec.Header().Values("Retry-After"); len(got) != 1 || got[0] != "7" {
		t.Errorf("Retry-After = %q, want exactly the single engine value 7", got)
	}
}

// retryAfterExempt lists the proxy-originated 429/503 refusals that carry no
// Retry-After on purpose, keyed by file, enclosing function and the error-code
// expression at the call, so a new refusal elsewhere cannot hide behind it.
var retryAfterExempt = map[string]string{
	"cloud_only.go:serveCloudOnly:CloudReasonLocalOnlyBlocked": "a local_only key on a cloud-only model has no honest value to give",
	"cloud_only.go:refuseCloudOnly:code":                       "cloud-only refusals (list unreadable, no cloud route, unsupported path) have no honest value to give",
	`proxy.go:ServeHTTP:"cloud_budget_exceeded"`:               "resets on a calendar boundary that needs an admin accessor to read; no honest value to give yet",
}

// TestEveryRetryableRefusalHasRetryAfter scans the non-test sources and
// requires each writeAPIError call answering 429 or 503 to follow a
// setRetryAfter / setNoNodesRetryAfter call in an enclosing block, or to be on
// the exempt list above.
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
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
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
			fn := ""
			for _, a := range stack {
				if fd, ok := a.(*ast.FuncDecl); ok {
					fn = fd.Name.Name
				}
			}
			if _, ok := retryAfterExempt[f+":"+fn+":"+code]; ok {
				return true
			}
			if !hasRetryAfterSetterBefore(stack, call) {
				t.Errorf("%s:%d: %s refusal (%s) has no Retry-After setter call before it and is not exempt",
					f, fset.Position(call.Pos()).Line, status, code)
			}
			return true
		})
	}
	// Conservative floor: the scan finds 9 calls today; far fewer means it broke.
	if seen < 6 {
		t.Errorf("scanner found only %d 429/503 writeAPIError calls; the scan is broken", seen)
	}
}

// hasRetryAfterSetterBefore reports whether a setRetryAfter or
// setNoNodesRetryAfter call expression appears in a statement that precedes
// call in any block enclosing it.
func hasRetryAfterSetterBefore(stack []ast.Node, call *ast.CallExpr) bool {
	for _, a := range stack {
		blk, ok := a.(*ast.BlockStmt)
		if !ok {
			continue
		}
		for _, st := range blk.List {
			if st.End() > call.Pos() {
				break
			}
			found := false
			ast.Inspect(st, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return !found
				}
				switch fun := c.Fun.(type) {
				case *ast.Ident:
					found = found || fun.Name == "setRetryAfter" || fun.Name == "setNoNodesRetryAfter"
				case *ast.SelectorExpr:
					found = found || fun.Sel.Name == "setNoNodesRetryAfter"
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}
