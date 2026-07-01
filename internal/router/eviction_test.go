package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

const mib = 1024 * 1024

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
	if err := r.unloadModel(context.Background(), n, "llama3"); err != nil {
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
	n := r.EvictForHeadroom(context.Background(), "n1", 40*mib)

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
	if n := r.EvictForHeadroom(context.Background(), "n1", 999*mib); n != 0 {
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
	if n := r.EvictForHeadroom(context.Background(), "n1", 40*mib); n != 0 {
		t.Errorf("evicted %d, want 0 (unknown VRAM)", n)
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
