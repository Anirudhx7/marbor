package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// unloadRecorder is an Ollama-shaped server that records which models were
// asked to unload (POST /api/generate with keep_alive 0), in order.
type unloadRecorder struct {
	mu     sync.Mutex
	models []string
	status int
}

func (u *unloadRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/generate" {
			http.NotFound(w, req)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(req.Body).Decode(&body)
		u.mu.Lock()
		u.models = append(u.models, body.Model)
		status := u.status
		u.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
}

func (u *unloadRecorder) got() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// evictFixture returns a router with one healthy Ollama node "n" holding the
// given loaded models, plus the unload recorder behind it.
func evictFixture(t *testing.T, totalMB int64, loaded []ModelInfo) (*Router, *NodeState, *unloadRecorder) {
	t.Helper()
	rec := &unloadRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n", URL: srv.URL, Runtime: "ollama"},
	}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Healthy = true
	n.VRAMTotalMB = totalMB
	n.LoadedModels = loaded
	n.mu.Unlock()
	return r, n, rec
}

func freeAfterOverhead(totalMB int64, used int64) int64 {
	return totalMB*1024*1024 - int64(float64(used)*FragmentationOverheadMult)
}

func TestEvictForHeadroomCreditsFreedBytesWithOverheadMultiplier(t *testing.T) {
	const gib = int64(1) << 30
	r, _, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "A", SizeVRAM: 4 * gib},
		{Name: "B", SizeVRAM: 4 * gib},
	})
	r.RecordModelUse("n", "B") // A is the coldest

	free0 := freeAfterOverhead(10240, 8*gib)
	// Exactly satisfiable once the freed bytes of model A are credited the
	// same way the used bytes were charged (with the overhead multiplier).
	freed := float64(4 * gib)
	needed := free0 + int64(freed*FragmentationOverheadMult) - 1

	evicted := r.EvictForHeadroom(context.Background(), "n", "X", needed)
	if evicted != 1 {
		t.Fatalf("evicted = %d (%v), want 1: freed bytes must be credited with the same overhead multiplier used when charging them", evicted, rec.got())
	}
}

func TestEvictForHeadroomSkipsVictimsWithNoKnownSize(t *testing.T) {
	const gib = int64(1) << 30
	r, _, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "unknown-size", SizeVRAM: 0}, // coldest, but unloading it frees nothing known
		{Name: "B", SizeVRAM: 4 * gib},
	})
	r.RecordModelUse("n", "B")

	needed := freeAfterOverhead(10240, 4*gib) + gib
	evicted := r.EvictForHeadroom(context.Background(), "n", "X", needed)
	got := rec.got()
	if evicted != 1 || len(got) != 1 || got[0] != "B" {
		t.Fatalf("evicted %d, unloaded %v; want only B (a zero-size victim frees nothing)", evicted, got)
	}
}

func TestEvictForHeadroomInFlightOnlyEvictsNothing(t *testing.T) {
	const gib = int64(1) << 30
	r, n, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "A", SizeVRAM: 4 * gib},
		{Name: "B", SizeVRAM: 4 * gib},
	})
	n.mu.Lock()
	n.modelInFlight = map[string]int32{"A": 1, "B": 2}
	n.mu.Unlock()
	logs := captureLog(t)

	evicted := r.EvictForHeadroom(context.Background(), "n", "X", 9*gib)
	if evicted != 0 || len(rec.got()) != 0 {
		t.Fatalf("evicted %d, unloaded %v; want none while every model is serving a request", evicted, rec.got())
	}
	if !strings.Contains(logs.String(), "in-flight") {
		t.Errorf("expected the in-flight shortfall to be logged, got %q", logs.String())
	}
}

func TestEvictForHeadroomPrefersModelsWithoutLiveSession(t *testing.T) {
	const gib = int64(1) << 30
	r, n, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "A", SizeVRAM: 4 * gib}, // coldest, but a live session uses it
		{Name: "B", SizeVRAM: 4 * gib},
	})
	r.RecordModelUse("n", "B")
	n.mu.RLock()
	nodeURL := n.URL
	n.mu.RUnlock()
	entry := &affinityEntry{nodeURL: nodeURL, model: "A"}
	entry.lastSeen.Store(time.Now().UnixNano())
	r.affinityMu.Lock()
	r.affinity["session-1"] = entry
	r.affinityMu.Unlock()

	needed := freeAfterOverhead(10240, 8*gib) + gib
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", needed); evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "B" {
		t.Fatalf("unloaded %v, want [B]: the model with a live sticky session is a last resort", got)
	}
}

func TestEvictForHeadroomStickySessionModelIsLastResort(t *testing.T) {
	const gib = int64(1) << 30
	r, n, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "A", SizeVRAM: 4 * gib},
		{Name: "B", SizeVRAM: 4 * gib},
	})
	n.mu.RLock()
	nodeURL := n.URL
	n.mu.RUnlock()
	for i, m := range []string{"A", "B"} {
		e := &affinityEntry{nodeURL: nodeURL, model: m}
		e.lastSeen.Store(time.Now().UnixNano())
		r.affinityMu.Lock()
		r.affinity[fmt.Sprintf("s%d", i)] = e
		r.affinityMu.Unlock()
	}
	r.RecordModelUse("n", "B")
	logs := captureLog(t)

	needed := freeAfterOverhead(10240, 8*gib) + gib
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", needed); evicted != 1 {
		t.Fatalf("evicted = %d, want 1 (last-resort eviction when every model has a session)", evicted)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "A" {
		t.Fatalf("unloaded %v, want [A] (the coldest)", got)
	}
	if !strings.Contains(logs.String(), "last resort") {
		t.Errorf("expected the last-resort eviction to be logged, got %q", logs.String())
	}
}

// --- model name equivalence (bare name vs latest tag) ---

func tagsServer(t *testing.T, sizes map[string]int64) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/tags" {
			http.NotFound(w, req)
			return
		}
		atomic.AddInt32(&hits, 1)
		type tm struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		var out struct {
			Models []tm `json:"models"`
		}
		for name, size := range sizes {
			out.Models = append(out.Models, tm{name, size})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestLastKnownVRAMMatchesBareAndTaggedNames(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.recordLastKnownVRAM("n", "llama3:latest", 1234)
	if got := r.lastKnownVRAMBytes("n", "llama3"); got != 1234 {
		t.Fatalf("lastKnownVRAMBytes(bare) = %d, want 1234 recorded under the tagged name", got)
	}
	r.recordLastKnownVRAM("n", "mistral", 99)
	if got := r.lastKnownVRAMBytes("n", "mistral:latest"); got != 99 {
		t.Fatalf("lastKnownVRAMBytes(tagged) = %d, want 99 recorded under the bare name", got)
	}
	if got := r.lastKnownVRAMBytes("n", "llama3:8b"); got != 0 {
		t.Fatalf("a different tag must not match, got %d", got)
	}
}

func TestLastUsedMatchesBareAndTaggedNames(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.RecordModelUse("n", "llama3")
	if r.lastUsedAt("n", "llama3:latest").IsZero() {
		t.Fatal("last-used stamp recorded for the bare name must be found by its latest-tag form")
	}
}

func TestEstimateModelSizeMatchesTaggedCatalogEntry(t *testing.T) {
	srv, _ := tagsServer(t, map[string]int64{"llama3:latest": 4 << 30})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	if got := r.estimateModelSizeBytes(srv.URL, "llama3", true, 0); got <= 0 {
		t.Fatalf("estimate for bare name = %d, want a size from the llama3:latest catalog entry", got)
	}
}

func TestModelDownloadedAnyNodeMatchesTaggedCatalogEntry(t *testing.T) {
	srv, _ := tagsServer(t, map[string]int64{"llama3:latest": 4 << 30})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	if !r.ModelDownloadedAnyNode("llama3") {
		t.Fatal("bare name must match the llama3:latest catalog entry")
	}
	if r.ModelDownloadedAnyNode("llama3:8b") {
		t.Fatal("a different tag must not match")
	}
}

func TestModelDownloadedAnyNodeCountsNodeNotMarkedHealthy(t *testing.T) {
	srv, _ := tagsServer(t, map[string]int64{"llama3:latest": 4 << 30})
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false // down, or not polled yet right after startup
	r.nodes[0].mu.Unlock()
	if !r.ModelDownloadedAnyNode("llama3:latest") {
		t.Fatal("a node that is not marked healthy yet must still count when its tags list the model")
	}
}

func TestModelDownloadedAnyNodeSkipsNodeWhoseTagsFail(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens: the tag fetch fails
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: url, Runtime: "ollama"}}, nil)
	if r.ModelDownloadedAnyNode("llama3:latest") {
		t.Fatal("a node whose tags cannot be fetched must not count as having the model")
	}
}

// --- agent unload URL / dispatch ---

func TestBuildAgentUnloadURL(t *testing.T) {
	cases := []struct {
		name, node, scheme, model, want string
		port                            int
		wantErr                         bool
	}{
		{name: "ipv4", node: "http://10.0.0.5:11434", port: 9200, model: "m", want: "http://10.0.0.5:9200/v1/models/m"},
		{name: "ipv6 loopback", node: "http://[::1]:11434", port: 9200, model: "m", want: "http://[::1]:9200/v1/models/m"},
		{name: "ipv6 global", node: "http://[2001:db8::10]:11434", port: 9200, scheme: "https", model: "m", want: "https://[2001:db8::10]:9200/v1/models/m"},
		{name: "slash model", node: "http://h:1", port: 9200, model: "org/repo:8b", want: "http://h:9200/v1/models/org/repo:8b"},
		{name: "no host", node: "http:///x", port: 9200, model: "m", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildAgentUnloadURL(c.node, c.port, c.scheme, c.model)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func agentUnloadRouter(t *testing.T, handler http.HandlerFunc) (*Router, MarborAgentConfig) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	r := New(config.RoutingConfig{}, nil, nil)
	return r, MarborAgentConfig{Enabled: true, Port: mustPort(t, srv.URL), Token: "tok", Scheme: "http"}
}

func TestUnloadModelViaAgentNon2xxIsFailureEvenWithOKTrue(t *testing.T) {
	r, cfg := agentUnloadRouter(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"ok":true}`))
	})
	if err := r.unloadModelViaAgent(context.Background(), "http://127.0.0.1:11434", cfg, "m"); err == nil {
		t.Fatal("a 500 reply must be a failure even when the body says ok:true")
	}
}

func TestUnloadModelViaAgentDecodeErrorKeepsCause(t *testing.T) {
	r, cfg := agentUnloadRouter(t, func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`<html>not json`))
	})
	err := r.unloadModelViaAgent(context.Background(), "http://127.0.0.1:11434", cfg, "m")
	var syn *json.SyntaxError
	if err == nil || !errors.As(err, &syn) {
		t.Fatalf("err = %v, want the JSON decode error wrapped", err)
	}
}

func TestUnloadModelViaAgentOK(t *testing.T) {
	r, cfg := agentUnloadRouter(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer tok" || req.Method != http.MethodPost || req.URL.Path != "/v1/models/m" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	if err := r.unloadModelViaAgent(context.Background(), "http://127.0.0.1:11434", cfg, "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnloadModelViaAgentReportsAgentError(t *testing.T) {
	r, cfg := agentUnloadRouter(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"ok":false,"error":"busy"}`))
	})
	err := r.unloadModelViaAgent(context.Background(), "http://127.0.0.1:11434", cfg, "m")
	if err == nil || err.Error() != "busy" {
		t.Fatalf("err = %v, want the agent's own message", err)
	}
}

func TestUnloadModelDirectNon2xxIsFailure(t *testing.T) {
	rec := &unloadRecorder{status: http.StatusNotModified}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)

	err := r.unloadModel(context.Background(), r.nodes[0], "m", "manual")
	if err == nil {
		t.Fatal("a non-2xx reply must not be reported as a successful unload")
	}
	if r.isWarmupSuppressed("n", "m") {
		t.Fatal("a failed unload must not run the success bookkeeping")
	}
}

func TestUnloadModelUnsupportedRuntime(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "v", URL: "http://127.0.0.1:8000", Runtime: "vllm"}}, nil)
	if err := r.unloadModel(context.Background(), r.nodes[0], "m", "manual"); !errors.Is(err, ErrUnloadUnsupported) {
		t.Fatalf("err = %v, want ErrUnloadUnsupported", err)
	}
}

// --- UnloadModels bookkeeping ---

func TestUnloadModelsAgentDownRecordsUnloadError(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434", Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Healthy = false
	n.AgentCapabilities = []string{"models.unload"}
	n.mu.Unlock()
	r.SetMarborAgent(n.Host, true, 9200, "tok", "http")

	err := r.UnloadModels(context.Background(), "n", []string{"m"})
	if err == nil {
		t.Fatal("want an error for a down node")
	}
	n.mu.RLock()
	msg := n.UnloadErrors["m"]
	n.mu.RUnlock()
	if !strings.Contains(msg, "unreachable") {
		t.Fatalf("UnloadErrors[m] = %q, want the fail-fast reason recorded for the dashboard", msg)
	}
}

func TestUnloadModelsPaths(t *testing.T) {
	rec := &unloadRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: srv.URL, Runtime: "ollama"}}, nil)
	n := r.nodes[0]
	ctx := context.Background()

	if err := r.UnloadModels(ctx, "ghost", []string{"m"}); err == nil {
		t.Error("unknown node must return an error")
	}

	// Pinned models are skipped without contacting the node.
	r.SetPinnedModels("n", []string{"pinned"})
	if err := r.UnloadModels(ctx, "n", []string{"pinned", ""}); err != nil || len(rec.got()) != 0 {
		t.Errorf("pinned/empty names: err=%v unloaded=%v, want nil and none", err, rec.got())
	}

	// A failure is recorded, returned and then cleared by a later success.
	rec.mu.Lock()
	rec.status = http.StatusInternalServerError
	rec.mu.Unlock()
	if err := r.UnloadModels(ctx, "n", []string{"m"}); err == nil || !strings.Contains(err.Error(), "m:") {
		t.Errorf("err = %v, want the failing model named", err)
	}
	n.mu.RLock()
	recorded := n.UnloadErrors["m"]
	n.mu.RUnlock()
	if recorded == "" {
		t.Error("failed unload must be recorded in UnloadErrors")
	}
	rec.mu.Lock()
	rec.status = http.StatusOK
	rec.mu.Unlock()
	if err := r.UnloadModels(ctx, "n", []string{"m"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	n.mu.RLock()
	_, still := n.UnloadErrors["m"]
	n.mu.RUnlock()
	if still {
		t.Error("a successful unload must clear the recorded error")
	}
	if !r.isWarmupSuppressed("n", "m") {
		t.Error("a scheduled unload must suppress warmup for the model")
	}
}

func TestUnloadModelsViaAgentSuccessClearsError(t *testing.T) {
	var calls int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer agent.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://127.0.0.1:11434", Runtime: "vllm"}}, nil)
	n := r.nodes[0]
	n.mu.Lock()
	n.Healthy = true
	n.AgentCapabilities = []string{"models.unload"}
	n.UnloadErrors = map[string]string{"m": "old failure"}
	n.mu.Unlock()
	r.SetMarborAgent(n.Host, true, mustPort(t, agent.URL), "tok", "http")

	if err := r.UnloadModels(context.Background(), "n", []string{"m"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", calls)
	}
	n.mu.RLock()
	_, still := n.UnloadErrors["m"]
	n.mu.RUnlock()
	if still {
		t.Error("success via the agent must clear the recorded error")
	}
}

// --- bounded per-node bookkeeping ---

func TestRecordModelUseIsBoundedPerNode(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.RecordModelUse("other", "keep-me")
	for i := 0; i < 5000; i++ {
		r.RecordModelUse("n", fmt.Sprintf("client-model-%d", i))
	}
	r.RecordModelUse("n", "latest-model")

	r.lruMu.Lock()
	count := 0
	for k := range r.lastUsed {
		if strings.HasPrefix(k, "n\x00") {
			count++
		}
	}
	r.lruMu.Unlock()
	if count > 1024 {
		t.Fatalf("tracked %d last-used entries for one node, want a bounded set (<= 1024)", count)
	}
	if r.lastUsedAt("n", "latest-model").IsZero() {
		t.Error("the most recently used model must survive trimming")
	}
	if r.lastUsedAt("other", "keep-me").IsZero() {
		t.Error("trimming one node must not touch another node's entries")
	}
}

func TestRecordLastKnownVRAMIsBoundedPerNode(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	for i := 0; i < 5000; i++ {
		r.recordLastKnownVRAM("n", fmt.Sprintf("model-%d", i), 100)
	}
	r.recordLastKnownVRAM("n", "latest-model", 7)
	r.vramSeenMu.Lock()
	count := len(r.lastKnownVRAM)
	r.vramSeenMu.Unlock()
	if count > 1024 {
		t.Fatalf("tracked %d size entries for one node, want <= 1024", count)
	}
	if r.lastKnownVRAMBytes("n", "latest-model") != 7 {
		t.Error("the newest entry must be kept")
	}
}
