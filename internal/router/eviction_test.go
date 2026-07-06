package router

import (
	"context"
	"errors"
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
