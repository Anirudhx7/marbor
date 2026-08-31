package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

const mib = 1024 * 1024

// TestUnloadModelManual verifies the operator-facing UnloadModel: it reports the
// node as found and sends a keep_alive:0 for the model, and reports not-found for
// an unknown node without erroring.
func TestUnloadModelManual(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{nodes: []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}}}

	found, err := r.UnloadModel(context.Background(), "n1", "llama3")
	if err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	if !found {
		t.Fatal("expected node n1 to be found")
	}
	if !strings.Contains(body, `"keep_alive":0`) || !strings.Contains(body, `"llama3"`) {
		t.Errorf("unload body missing keep_alive:0 or model: %s", body)
	}

	found, err = r.UnloadModel(context.Background(), "ghost", "llama3")
	if err != nil {
		t.Errorf("unknown node should not error, got %v", err)
	}
	if found {
		t.Error("unknown node should report found=false")
	}
}

// TestUnloadModelManualPinnedRejected verifies that a pinned model cannot be
// unloaded via the manual/operator UnloadModel path: pinning means "never
// evict or unload without an explicit unpin first", and that guarantee must
// hold on every unload path, not just auto-eviction. No request should reach
// the node at all.
func TestUnloadModelManualPinnedRejected(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		pinned: map[string]map[string]bool{},
	}
	r.SetPinnedModels("n1", []string{"guarded-model"})

	found, err := r.UnloadModel(context.Background(), "n1", "guarded-model")
	if !found {
		t.Fatal("expected node n1 to be found")
	}
	if !errors.Is(err, ErrModelPinned) {
		t.Fatalf("UnloadModel err = %v, want ErrModelPinned", err)
	}
	if hit {
		t.Error("pinned model unload must not contact the node at all")
	}
}

// TestUnloadModelManualNonPinnedStillWorks verifies the fix doesn't regress
// the existing behavior: unloading a model that isn't pinned still sends
// keep_alive:0 exactly as before.
func TestUnloadModelManualNonPinnedStillWorks(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		pinned: map[string]map[string]bool{},
	}
	r.SetPinnedModels("n1", []string{"other-model"}) // pinned set is non-empty, but not this model

	found, err := r.UnloadModel(context.Background(), "n1", "llama3")
	if err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	if !found {
		t.Fatal("expected node n1 to be found")
	}
	if !strings.Contains(body, `"keep_alive":0`) || !strings.Contains(body, `"llama3"`) {
		t.Errorf("unload body missing keep_alive:0 or model: %s", body)
	}
}

// TestUnloadModelsScheduledSkipsPinned verifies the scheduled "unload" action
// shares the same pinned-model guard as the manual path: a pinned model in the
// schedule's model list is skipped (never sent to the node) while sibling
// non-pinned models still unload normally.
func TestUnloadModelsScheduledSkipsPinned(t *testing.T) {
	var mu sync.Mutex
	got := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		for _, m := range []string{"a", "guarded"} {
			if strings.Contains(string(b), `"`+m+`"`) && strings.Contains(string(b), `"keep_alive":0`) {
				got[m] = true
			}
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		pinned: map[string]map[string]bool{},
	}
	r.SetPinnedModels("n1", []string{"guarded"})

	r.UnloadModels(context.Background(), "n1", []string{"a", "guarded"})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !got["a"] {
		t.Error("expected non-pinned model 'a' to be unloaded")
	}
	if got["guarded"] {
		t.Error("pinned model 'guarded' must not be unloaded by the scheduled action")
	}
}

// TestUnloadModelsScheduled verifies the scheduled multi-model unload fires a
// keep_alive:0 per model on the target node.
func TestUnloadModelsScheduled(t *testing.T) {
	var mu sync.Mutex
	got := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		for _, m := range []string{"a", "b"} {
			if strings.Contains(string(b), `"`+m+`"`) && strings.Contains(string(b), `"keep_alive":0`) {
				got[m] = true
			}
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{nodes: []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}}}
	r.UnloadModels(context.Background(), "n1", []string{"a", "b", ""})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !got["a"] || !got["b"] {
		t.Errorf("expected both models unloaded, got %v", got)
	}
}

// TestUnloadModelsScheduledDispatchesToAgentWhenCapable verifies the P33 fix:
// a scheduled unload on a node whose agent reports "models.unload" dispatches
// through the agent (POST /v1/models/{name}) instead of the direct Ollama
// keep_alive:0 HTTP call, mirroring handleUnloadModel's manual-path behavior.
func TestUnloadModelsScheduledDispatchesToAgentWhenCapable(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotPath, gotAuth string
	directHit := false
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer agentSrv.Close()

	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHit = true
		w.Write([]byte(`{"done":true}`))
	}))
	defer nodeSrv.Close()

	var agentPort int
	fmt.Sscanf(strings.TrimPrefix(agentSrv.URL, "http://127.0.0.1:"), "%d", &agentPort)

	// NodeState is constructed directly here (bypassing AddNode, which would
	// otherwise default Host from the URL's hostname) - set Host explicitly
	// so it matches the key SetMarborAgent is called with below (marborAgents is
	// keyed by Host, not Name - see SetMarborAgent's doc comment).
	r := &Router{nodes: []*NodeState{{Name: "n1", URL: nodeSrv.URL, Host: "n1", Healthy: true}}}
	r.SetMarborAgent("n1", true, agentPort, "agent-secret-token", "http")
	r.nodes[0].AgentCapabilities = []string{"models.unload"}

	r.UnloadModels(context.Background(), "n1", []string{"org/repo"})
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("expected agent request method POST, got %q", gotMethod)
	}
	if gotPath != "/v1/models/org/repo" {
		t.Errorf("expected agent request path /v1/models/org/repo, got %q", gotPath)
	}
	if gotAuth != "Bearer agent-secret-token" {
		t.Errorf("agent request Authorization = %q, want Bearer agent-secret-token", gotAuth)
	}
	if directHit {
		t.Error("expected the direct Ollama node to never be contacted when the agent is capable")
	}
}

// TestUnloadModelsScheduledAgentDownNodeSkipped verifies a scheduled unload
// fails fast (never contacts the agent) when the node's poller-tracked
// health is currently down, mirroring handleUnloadModel's agent-branch
// fail-fast check.
func TestUnloadModelsScheduledAgentDownNodeSkipped(t *testing.T) {
	hit := false
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer agentSrv.Close()

	var agentPort int
	fmt.Sscanf(strings.TrimPrefix(agentSrv.URL, "http://127.0.0.1:"), "%d", &agentPort)

	r := &Router{nodes: []*NodeState{{Name: "n1", URL: "http://localhost:11434", Host: "n1", Healthy: false}}}
	r.SetMarborAgent("n1", true, agentPort, "agent-secret-token", "http")
	r.nodes[0].AgentCapabilities = []string{"models.unload"}

	r.UnloadModels(context.Background(), "n1", []string{"some-model"})
	time.Sleep(150 * time.Millisecond)

	if hit {
		t.Error("agent must not be contacted for a down node")
	}
}

// TestUnloadModelsScheduledNoAgentCapabilityUsesDirectPath verifies that a
// node with an agent enabled but WITHOUT "models.unload" in its reported
// capabilities still falls back to the direct Ollama path unchanged -
// reliability requirement: only "models.unload" specifically gates the
// agent dispatch, not agent presence alone.
func TestUnloadModelsScheduledNoAgentCapabilityUsesDirectPath(t *testing.T) {
	var mu sync.Mutex
	var body string
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer nodeSrv.Close()

	r := &Router{nodes: []*NodeState{{Name: "n1", URL: nodeSrv.URL, Host: "n1", Healthy: true}}}
	r.SetMarborAgent("n1", true, 9999, "agent-secret-token", "http")
	r.nodes[0].AgentCapabilities = []string{"status", "models.pull"} // no "models.unload"

	r.UnloadModels(context.Background(), "n1", []string{"llama3"})
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(body, `"keep_alive":0`) || !strings.Contains(body, `"llama3"`) {
		t.Errorf("expected direct keep_alive:0 unload, got body: %s", body)
	}
}

// TestBuildAgentUnloadURL_EscapesReservedCharacters mirrors admin package's
// equivalent test for its own copy of this URL builder.
func TestBuildAgentUnloadURL_EscapesReservedCharacters(t *testing.T) {
	cases := map[string]string{
		"org/repo":     "http://localhost:9911/v1/models/org/repo",
		"org/repo#tag": "http://localhost:9911/v1/models/org/repo%23tag",
		"org/repo?x=1": "http://localhost:9911/v1/models/org/repo%3Fx=1",
		"org/my repo":  "http://localhost:9911/v1/models/org/my%20repo",
	}
	for model, want := range cases {
		got, err := buildAgentUnloadURL("http://localhost:11434", 9911, "http", model)
		if err != nil {
			t.Fatalf("buildAgentUnloadURL(%q): %v", model, err)
		}
		if got != want {
			t.Errorf("buildAgentUnloadURL(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestUnloadModelSendsKeepAliveZero verifies eviction hits /api/generate with
// keep_alive:0 (the real Ollama unload).
func TestUnloadModelSendsKeepAliveZero(t *testing.T) {
	var body string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, path = string(b), r.URL.Path
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{}
	n := &NodeState{Name: "n1", URL: srv.URL, Healthy: true}
	if err := r.unloadModel(context.Background(), n, "llama3", "test"); err != nil {
		t.Fatalf("unloadModel: %v", err)
	}
	if path != "/api/generate" {
		t.Errorf("unload path = %q, want /api/generate", path)
	}
	if !strings.Contains(body, `"keep_alive":0`) {
		t.Errorf("unload body missing keep_alive:0: %s", body)
	}
	if !strings.Contains(body, `"llama3"`) {
		t.Errorf("unload body missing model: %s", body)
	}
}

// TestUnloadModelNonOllamaReturnsSentinelError is the P101 regression: a
// known non-Ollama runtime must return ErrUnloadUnsupported, not nil - a nil
// return was indistinguishable from a genuine successful unload to every
// caller (manual unload endpoint, UnloadModels' direct-path fallback,
// EvictForHeadroom's free-byte accounting), silently booking a phantom
// eviction that never freed any VRAM.
func TestUnloadModelNonOllamaReturnsSentinelError(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{}
	n := &NodeState{Name: "n1", URL: srv.URL, Healthy: true, Runtime: "vllm"}
	err := r.unloadModel(context.Background(), n, "llama3", "test")
	if !errors.Is(err, ErrUnloadUnsupported) {
		t.Errorf("unloadModel err = %v, want ErrUnloadUnsupported", err)
	}
	if called {
		t.Error("unloadModel should not contact a non-Ollama node's HTTP endpoint at all")
	}
}

// TestEvictForHeadroomEvictsColdestNonPinned verifies coldest-first eviction,
// pinned protection, and stopping once enough VRAM is free.
func TestEvictForHeadroomEvictsColdestNonPinned(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// crude: extract the model name from the JSON body
		mu.Lock()
		for _, m := range []string{"cold", "warm", "pinned"} {
			if strings.Contains(string(b), `"`+m+`"`) {
				evicted[m] = true
			}
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: srv.URL, Healthy: true,
			VRAMTotalMB: 120, // ~125.8 MB total
			LoadedModels: []ModelInfo{
				{Name: "cold", SizeVRAM: 40 * mib},
				{Name: "warm", SizeVRAM: 40 * mib},
				{Name: "pinned", SizeVRAM: 40 * mib},
			},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	now := time.Now()
	r.lastUsed[modelKey("n1", "cold")] = now.Add(-2 * time.Hour)
	r.lastUsed[modelKey("n1", "warm")] = now.Add(-1 * time.Minute)
	r.SetPinnedModels("n1", []string{"pinned"})

	// used = 120 MB, total ~125.8 MB, free ~5.8 MB. Need 40 MB → must evict.
	n := r.EvictForHeadroom(context.Background(), "n1", "newmodel", 40*mib)

	if n < 1 {
		t.Fatalf("expected at least 1 eviction, got %d", n)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !evicted["cold"] {
		t.Error("coldest model 'cold' should have been evicted first")
	}
	if evicted["pinned"] {
		t.Error("pinned model must never be evicted")
	}
}

// TestEvictForHeadroomStopsWhenOnlyPinned verifies pinned-only nodes evict
// nothing (and don't loop forever).
func TestEvictForHeadroomStopsWhenOnlyPinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()
	r := &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: srv.URL, Healthy: true, VRAMTotalMB: 50,
			LoadedModels: []ModelInfo{{Name: "onlypinned", SizeVRAM: 40 * mib}},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	r.SetPinnedModels("n1", []string{"onlypinned"})
	if n := r.EvictForHeadroom(context.Background(), "n1", "newmodel", 999*mib); n != 0 {
		t.Errorf("evicted %d, want 0 (only pinned present)", n)
	}
}

// TestEvictForHeadroomUnknownVRAMNoop verifies no eviction when capacity unknown.
func TestEvictForHeadroomUnknownVRAMNoop(t *testing.T) {
	r := &Router{
		nodes:    []*NodeState{{Name: "n1", URL: "http://x", Healthy: true, VRAMTotalMB: 0, LoadedModels: []ModelInfo{{Name: "a", SizeVRAM: 40 * mib}}}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	if n := r.EvictForHeadroom(context.Background(), "n1", "a", 40*mib); n != 0 {
		t.Errorf("evicted %d, want 0 (unknown VRAM)", n)
	}
}

// TestEvictForHeadroomProtectsHigherPriorityKeepWarmModel verifies the
// keep-warm priority hierarchy: when a node can't fit two keep-warm models
// together, EvictForHeadroom must never evict the higher-priority one (lower
// rank, e.g. first in the configured "keep warm" list) to make room for a
// lower-priority one. Regression test for the reported bug where two
// always-warm models on a VRAM-constrained node flipped resident/evicted at
// random because eviction only used LRU with no notion of priority.
func TestEvictForHeadroomProtectsHigherPriorityKeepWarmModel(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		for _, m := range []string{"high-priority", "low-priority"} {
			if strings.Contains(string(b), `"`+m+`"`) {
				evicted[m] = true
			}
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: srv.URL, Healthy: true,
			VRAMTotalMB: 50, // fully consumed by high-priority alone
			LoadedModels: []ModelInfo{
				{Name: "high-priority", SizeVRAM: 50 * mib},
			},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	r.setWarmPriority("n1", []string{"high-priority", "low-priority"})

	// low-priority needs 40MB; only the higher-priority model is loaded, and
	// it must not be sacrificed for a lower-priority one.
	n := r.EvictForHeadroom(context.Background(), "n1", "low-priority", 40*mib)
	if n != 0 {
		t.Fatalf("expected 0 evictions (only a higher-priority keep-warm model present), got %d", n)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if evicted["high-priority"] {
		t.Error("higher-priority keep-warm model must never be evicted for a lower-priority one")
	}
}

// TestEvictForHeadroomLowerPriorityKeepWarmModelIsEvictable verifies the
// inverse of the priority-protection test above: a lower-priority keep-warm
// model IS evictable to make room for a higher-priority one.
func TestEvictForHeadroomLowerPriorityKeepWarmModelIsEvictable(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(b), `"low-priority"`) {
			evicted["low-priority"] = true
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: srv.URL, Healthy: true,
			VRAMTotalMB: 50,
			LoadedModels: []ModelInfo{
				{Name: "low-priority", SizeVRAM: 50 * mib},
			},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	r.setWarmPriority("n1", []string{"high-priority", "low-priority"})

	n := r.EvictForHeadroom(context.Background(), "n1", "high-priority", 40*mib)
	if n < 1 {
		t.Fatalf("expected low-priority model to be evicted for high-priority one, got %d evictions", n)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !evicted["low-priority"] {
		t.Error("lower-priority keep-warm model should be evictable to make room for a higher-priority one")
	}
}

// TestEnsureHeadroomEvictsWhenModelWontFit verifies the load-path gate evicts the
// coldest model when a not-yet-loaded model's estimated size won't fit.
func TestEnsureHeadroomEvictsWhenModelWontFit(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"newmodel","size":41943040}]}`)) // 40 MiB on disk
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(b), `"cold"`) {
			evicted["cold"] = true
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: srv.URL}}, nil)
	n := r.nodes[0]
	n.Healthy = true
	n.VRAMTotalMB = 100                                                                                  // ~104.8 MB
	n.LoadedModels = []ModelInfo{{Name: "cold", SizeVRAM: 50 * mib}, {Name: "warm", SizeVRAM: 40 * mib}} // 90 MB used
	r.lastUsed[modelKey("n1", "cold")] = time.Now().Add(-time.Hour)
	r.lastUsed[modelKey("n1", "warm")] = time.Now()

	// free ~14.8 MB, newmodel needs 40 MB -> must evict the coldest ("cold").
	r.ensureHeadroom(context.Background(), n, "newmodel")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !evicted["cold"] {
		t.Error("ensureHeadroom should have evicted coldest 'cold' to fit newmodel")
	}
}

// TestEstimateModelSizeBytesPrefersLastKnownRealVRAM reproduces the reported
// bug: a large quantized model that doesn't fully fit in VRAM (Ollama splits
// it across GPU+CPU) reports a real /api/ps size_vram far smaller than its
// on-disk weights size (an 8B Q4_K_M model was ~9.6GB on disk but only used
// ~3.3GB of real VRAM). Before this model has ever been observed loaded,
// estimateModelSizeBytes has nothing but the disk size to go on. But once a
// poll has confirmed its real footprint, every later headroom/reservation
// calculation must use that real number - continuing to use the disk size
// forever inflated every sibling model's headroom math without bound, making
// a tiny model (a few hundred MB) look permanently unfittable on a node that
// actually had room for it.
func TestEstimateModelSizeBytesPrefersLastKnownRealVRAM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"gemma4:latest","size":9608350718}]}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "pve", URL: srv.URL}}, nil)

	// Never observed loaded yet: falls back to the on-disk size.
	if got := r.estimateModelSizeBytes(srv.URL, "gemma4:latest", true); got != 9608350718 {
		t.Fatalf("before any observation, estimate = %d, want on-disk size 9608350718", got)
	}

	// A poll confirms the real, much smaller VRAM footprint.
	r.recordLastKnownVRAM("pve", "gemma4:latest", 3327739904)

	if got := r.estimateModelSizeBytes(srv.URL, "gemma4:latest", true); got != 3327739904 {
		t.Fatalf("after observing real VRAM usage, estimate = %d, want real size 3327739904", got)
	}
}

// TestEnsureHeadroomUsesRealVRAMNotDiskSizeForSiblingReservation is the
// end-to-end version of the same bug: with a node too small to hold BOTH
// models at their on-disk sizes but big enough for their real VRAM
// footprints, a sibling model's headroom check must not be blocked by an
// in-flight reservation that (wrongly) used the other model's disk size.
func TestEnsureHeadroomUsesRealVRAMNotDiskSizeForSiblingReservation(t *testing.T) {
	var evicted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"mxbai","size":671088640}]}`)) // 640 MiB on disk
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"keep_alive":0`) {
			evicted = true
		}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "pve", URL: srv.URL}}, nil)
	n := r.nodes[0]
	n.Healthy = true
	n.VRAMTotalMB = 4096 // 4 GiB total

	// "gemma" is mid-reload (not yet resident): its own headroom check
	// reserves against its estimate. Its disk size (9GiB) alone exceeds the
	// entire node, but its real, previously-observed VRAM footprint (3200
	// MiB) does not - the reservation must use the real figure.
	r.recordLastKnownVRAM("pve", "gemma", 3200*mib)
	r.reserveWarmBytes("pve", "gemma", r.estimateModelSizeBytes(srv.URL, "gemma", true))

	// mxbai (640 MiB on disk, never loaded before) should still fit in the
	// ~896 MiB left over (4096 - 3200 MiB), not be blocked by a phantom 9GiB
	// reservation for gemma. Nothing is actually resident yet (both mid-load),
	// so there is nothing to evict either.
	n.LoadedModels = nil
	r.ensureHeadroom(context.Background(), n, "mxbai")

	if evicted {
		t.Error("ensureHeadroom triggered an eviction - mxbai should have fit in the real 896 MiB free without evicting anything")
	}
	const want = 3200*mib + 640*mib // gemma's real reservation + mxbai's own
	if got := r.PendingPrewarmBytes("pve"); got != want {
		t.Fatalf("pve pending reservations = %d, want %d - gemma's 9GiB disk size leaked into the total instead of its real 3200 MiB", got, want)
	}
}

// TestReserveWarmBytesAccountsForInFlightSiblings verifies that a live
// reservation for one model on a node is visible (as "bytes reserved by
// others") to a subsequent reservation for a different model on the same
// node, is scoped per-node, and disappears once cleared. This is the
// bookkeeping that lets ensureHeadroom see a sibling warmup's in-flight load
// even though the poller hasn't confirmed it resident yet.
func TestReserveWarmBytesAccountsForInFlightSiblings(t *testing.T) {
	r := &Router{}

	if others := r.reserveWarmBytes("n1", "llama3", 40*mib); others != 0 {
		t.Fatalf("first reservation should see no siblings, got %d", others)
	}
	if others := r.reserveWarmBytes("n1", "mistral", 30*mib); others != 40*mib {
		t.Fatalf("second reservation should see llama3's 40MiB in flight, got %d", others)
	}
	// A reservation on a different node must not leak into this node's total.
	if others := r.reserveWarmBytes("n2", "llama3", 999*mib); others != 0 {
		t.Fatalf("reservation on a different node should not see n1's reservations, got %d", others)
	}
	r.clearWarmReservation("n1", "llama3")
	if others := r.reserveWarmBytes("n1", "gemma", 10*mib); others != 30*mib {
		t.Fatalf("after clearing llama3, only mistral's 30MiB should remain, got %d", others)
	}
}

// TestPendingPrewarmBytes_SumsLiveReservations verifies that
// PendingPrewarmBytes reports the real, live warm-reservation bookkeeping -
// the same data used for headroom accounting - scoped per node, and that a
// cleared reservation no longer contributes.
func TestPendingPrewarmBytes_SumsLiveReservations(t *testing.T) {
	r := &Router{}

	if got := r.PendingPrewarmBytes("n1"); got != 0 {
		t.Fatalf("no reservations yet: got %d, want 0", got)
	}

	r.reserveWarmBytes("n1", "llama3", 40*mib)
	r.reserveWarmBytes("n1", "mistral", 30*mib)
	r.reserveWarmBytes("n2", "gemma", 999*mib)

	if got := r.PendingPrewarmBytes("n1"); got != 70*mib {
		t.Errorf("n1 pending = %d, want %d (40+30 MiB)", got, 70*mib)
	}
	if got := r.PendingPrewarmBytes("n2"); got != 999*mib {
		t.Errorf("n2 pending = %d, want %d", got, 999*mib)
	}

	r.clearWarmReservation("n1", "llama3")
	if got := r.PendingPrewarmBytes("n1"); got != 30*mib {
		t.Errorf("after clearing llama3, n1 pending = %d, want %d (mistral only)", got, 30*mib)
	}
}

// TestPendingPrewarmBytes_ExpiredReservationExcluded verifies that a
// reservation older than warmReservationTTL is treated as decayed and does
// not contribute to the reported pending total, matching the same TTL used
// by reserveWarmBytes for headroom accounting.
func TestPendingPrewarmBytes_ExpiredReservationExcluded(t *testing.T) {
	r := &Router{}
	r.reserveWarmBytes("n1", "llama3", 40*mib)

	r.evictMu.Lock()
	stale := r.warmReserved["n1"]["llama3"]
	stale.at = time.Now().Add(-warmReservationTTL - time.Minute)
	r.warmReserved["n1"]["llama3"] = stale
	r.evictMu.Unlock()

	if got := r.PendingPrewarmBytes("n1"); got != 0 {
		t.Errorf("expired reservation should be excluded: got %d, want 0", got)
	}
}

// TestModelFitsAnyHealthyNode covers the three real-data cases: fits, doesn't
// fit anywhere, and unknown size on a node with KNOWN capacity (fails open -
// never guessed). See TestModelFitsAnyHealthyNode_NoCapacityKnownFailsClosed
// below for the P403 case where capacity itself is unknown fleet-wide.
func TestModelFitsAnyHealthyNode(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.nodes[0].Healthy = true
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000
	r.tagsCache["http://localhost:11434"] = &TagsCache{
		Models: []TagModel{
			{Name: "big-model", Size: 20000 * mib},  // 20GB > 14GB free - doesn't fit
			{Name: "small-model", Size: 4000 * mib}, // 4GB < 14GB free - fits
		},
		FetchedAt: time.Now(),
	}

	if !r.ModelFitsAnyHealthyNode("small-model") {
		t.Error("small-model should fit in 14GB free VRAM")
	}
	if r.ModelFitsAnyHealthyNode("big-model") {
		t.Error("big-model (20GB) should not fit in 14GB free VRAM")
	}
	if !r.ModelFitsAnyHealthyNode("unknown-model") {
		t.Error("a model with no known size anywhere, but known capacity, should fail open (fit=true) - never guess")
	}
}

// TestModelFitsAnyHealthyNode_NoCapacityKnownFailsClosed covers P403 (audit
// H4): a remote fleet with no marbor-agent and no manually declared
// vram_total_mb has VRAMTotalMB==0 on every node (health.go's "api"-source
// path only ever populates real used-VRAM, never a total). Before the fix,
// this made ModelFitsAnyHealthyNode's sawKnownSize stay false with zero
// nodes even examined for size, so it fell open to "fits" - a 70B model
// would get treated as fitting on a node with literally zero real capacity
// signal. The fix must distinguish "no capacity signal at all" (fail CLOSED,
// false) from "capacity known, size unknown" (fail open, true - unchanged
// behavior covered by TestModelFitsAnyHealthyNode above).
func TestModelFitsAnyHealthyNode_NoCapacityKnownFailsClosed(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434"},
	}, nil)
	r.nodes[0].Healthy = true
	// Simulate the health.go "api" source path: real used-VRAM from /api/ps,
	// but no nvidia-smi, no agent, no declared vram_total_mb - so
	// VRAMTotalMB stays 0, exactly as health.go's default case leaves it.
	r.nodes[0].VRAMUsedMB = 2000
	r.nodes[0].VRAMTotalMB = 0
	r.nodes[0].VRAMSource = "api"
	r.tagsCache["http://localhost:11434"] = &TagsCache{
		Models: []TagModel{
			{Name: "big-model-70b", Size: 40000 * mib},
		},
		FetchedAt: time.Now(),
	}

	if r.ModelFitsAnyHealthyNode("big-model-70b") {
		t.Error("a fleet with zero known VRAM capacity must not report fit=true (P403 fail-open bug) - even though the model's size is known, there is no real capacity signal to compare it against")
	}
}

// TestModelDownloadedAnyNode verifies presence is checked against real
// /api/tags data, not guessed.
func TestModelDownloadedAnyNode(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434"},
	}, nil)
	r.tagsCache["http://localhost:11434"] = &TagsCache{
		Models:    []TagModel{{Name: "llama3.1:70b-q4_K_M", Size: 4000 * mib}},
		FetchedAt: time.Now(),
	}

	if !r.ModelDownloadedAnyNode("llama3.1:70b-q4_K_M") {
		t.Error("expected llama3.1:70b-q4_K_M to be reported as downloaded")
	}
	if r.ModelDownloadedAnyNode("llama3.1:70b-q3_K_M") {
		t.Error("expected llama3.1:70b-q3_K_M (not in tags) to be reported as not downloaded")
	}
}

// TestFallbackChainFor verifies the config-only, immutable-after-construction
// accessor: declared models return their chain, undeclared models return nil.
func TestFallbackChainFor(t *testing.T) {
	r := New(config.RoutingConfig{
		Strategy:       "warm-first",
		FallbackChains: map[string][]string{"llama3.1:70b": {"llama3.1:70b-q4_K_M", "llama3.1:70b-q3_K_M"}},
	}, nil, nil)

	if got := r.FallbackChainFor("llama3.1:70b"); len(got) != 2 || got[0] != "llama3.1:70b-q4_K_M" {
		t.Errorf("FallbackChainFor(declared) = %v, want [llama3.1:70b-q4_K_M llama3.1:70b-q3_K_M]", got)
	}
	if got := r.FallbackChainFor("undeclared-model"); got != nil {
		t.Errorf("FallbackChainFor(undeclared) = %v, want nil", got)
	}
}

// TestLocalDegradationChainFor verifies the config-only, immutable-after-
// construction accessor for P67's local degradation chain: declared models
// return their chain, undeclared models return nil.
func TestLocalDegradationChainFor(t *testing.T) {
	r := New(config.RoutingConfig{
		Strategy:               "warm-first",
		LocalDegradationChains: map[string][]string{"big-model": {"small-model", "tiny-model"}},
	}, nil, nil)

	if got := r.LocalDegradationChainFor("big-model"); len(got) != 2 || got[0] != "small-model" {
		t.Errorf("LocalDegradationChainFor(declared) = %v, want [small-model tiny-model]", got)
	}
	if got := r.LocalDegradationChainFor("undeclared-model"); got != nil {
		t.Errorf("LocalDegradationChainFor(undeclared) = %v, want nil", got)
	}
}

// TestEnsureHeadroomAccountsForConcurrentSiblingLoad verifies the fix for the
// warmup headroom race: when one model's warmup is already in flight
// (reserved, but not yet confirmed by a poll) on a node, a second model's
// headroom check must account for it. Without this, ensureHeadroom would read
// the stale, pre-warmup LoadedModels snapshot, believe it has the whole node
// to itself, and skip eviction even though the two loads together won't fit -
// leaving the real runtime to arbitrate, which is how a node ends up with
// only one of two warmed models actually resident.
func TestEnsureHeadroomAccountsForConcurrentSiblingLoad(t *testing.T) {
	var mu sync.Mutex
	evicted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"mistral","size":20971520}]}`)) // 20 MiB on disk
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(b), `"old"`) {
			evicted["old"] = true
		}
		mu.Unlock()
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: srv.URL}}, nil)
	n := r.nodes[0]
	n.Healthy = true
	n.VRAMTotalMB = 50 // 50 MiB total
	n.LoadedModels = []ModelInfo{{Name: "old", SizeVRAM: 20 * mib}}
	r.lastUsed[modelKey("n1", "old")] = time.Now().Add(-time.Hour)

	// Simulate llama3's warmup already in flight (reserved, not yet polled):
	// free = 50-20 = 30MiB before considering it. mistral needs 20MiB, which
	// looks like it fits on the stale snapshot alone (30 >= 20). Once
	// llama3's 15MiB in-flight reservation is subtracted, only 15MiB remains
	// free, which is NOT enough for mistral - eviction must kick in.
	r.reserveWarmBytes("n1", "llama3", 15*mib)

	r.ensureHeadroom(context.Background(), n, "mistral")
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !evicted["old"] {
		t.Error("ensureHeadroom should have evicted 'old' once llama3's in-flight reservation was accounted for")
	}
}

// TestEnsureHeadroomClearsReservationOnceResident verifies that once a model
// is confirmed resident by a poll, its in-flight reservation is dropped so it
// stops double-counting against the (now real) usedBytes on a later check for
// a sibling model - otherwise a model that finished loading a while ago would
// permanently look like it's using its VRAM twice.
func TestEnsureHeadroomClearsReservationOnceResident(t *testing.T) {
	r := &Router{
		nodes: []*NodeState{{
			Name: "n1", Healthy: true, VRAMTotalMB: 50,
			LoadedModels: []ModelInfo{{Name: "llama3", SizeVRAM: 15 * mib}},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	n := r.nodes[0]
	// A stale reservation lingers from before the poll confirmed residency.
	r.reserveWarmBytes("n1", "llama3", 15*mib)

	// ensureHeadroom should see llama3 as resident (real LoadedModels) and
	// clear its stale reservation as a side effect, without evicting anything.
	r.ensureHeadroom(context.Background(), n, "llama3")

	others := r.reserveWarmBytes("n1", "mistral", 10*mib)
	if others != 0 {
		t.Errorf("llama3's reservation should have been cleared once resident, but sibling check still saw %d bytes", others)
	}
}

// TestEnsureHeadroomNoopWhenFits verifies no eviction when the model already fits.
func TestEnsureHeadroomNoopWhenFits(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"newmodel","size":41943040}]}`))
			return
		}
		hit = true // an /api/generate call here would be an unload
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: srv.URL}}, nil)
	n := r.nodes[0]
	n.Healthy = true
	n.VRAMTotalMB = 500 // plenty
	n.LoadedModels = []ModelInfo{{Name: "warm", SizeVRAM: 40 * mib}}

	r.ensureHeadroom(context.Background(), n, "newmodel")
	time.Sleep(100 * time.Millisecond)
	if hit {
		t.Error("ensureHeadroom evicted despite ample free VRAM")
	}
}

// TestReserveColdStartBytes_NeverFetchesOverHTTP guards P51's core constraint:
// the hot request-routing path (Route/selectBestNode/RouteExcluding) must
// never block on I/O (R2). reserveColdStartBytes must use only already-known,
// zero-I/O size data - never estimateModelSizeBytes's HTTP-fetch fallback -
// even when a live /api/tags endpoint would happily answer.
func TestReserveColdStartBytes_NeverFetchesOverHTTP(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		w.Write([]byte(`{"models":[{"name":"model-x","size":9999999}]}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: srv.URL}}, nil)

	// No lastKnownVRAM, no VRAMOverrides recorded: size is genuinely unknown
	// via any zero-I/O path, even though the on-disk size IS available over
	// HTTP. Must never call the server to find out (R2), and per P402 must
	// fall back to the unknownModelReserveBytes placeholder guard rather than
	// reserving nothing - reserving 0 here is exactly the fail-open bug that
	// let concurrent cold starts for a never-seen model double-book a node.
	r.reserveColdStartBytes(srv.URL, "n1", "model-x")

	if fetched {
		t.Error("reserveColdStartBytes made an HTTP call - the hot request path must never block on I/O (R2)")
	}
	if got := r.PendingPrewarmBytes("n1"); got != unknownModelReserveBytes {
		t.Errorf("PendingPrewarmBytes(n1) = %d, want %d (unknown size must reserve the P402 placeholder guard, not nothing)", got, unknownModelReserveBytes)
	}
}

// TestReserveColdStartBytes_UsesKnownSizeWithoutFetching verifies the
// zero-I/O estimate path (lastKnownVRAM) still results in a real reservation
// once residency has been observed at least once - the whole point of
// reserving on the hot path is to protect repeat cold-starts of a
// previously-seen model, which is the common case in practice.
func TestReserveColdStartBytes_UsesKnownSizeWithoutFetching(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		w.Write([]byte(`{"models":[{"name":"model-x","size":9999999}]}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: srv.URL}}, nil)
	r.recordLastKnownVRAM("n1", "model-x", 4000*mib)

	r.reserveColdStartBytes(srv.URL, "n1", "model-x")

	if fetched {
		t.Error("reserveColdStartBytes made an HTTP call despite a known lastKnownVRAM size - must prefer the zero-I/O source")
	}
	if got := r.PendingPrewarmBytes("n1"); got != 4000*mib {
		t.Errorf("PendingPrewarmBytes(n1) = %d, want %d (the real lastKnownVRAM figure)", got, 4000*mib)
	}
}
