package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func newTaggedRouter(runtime string, loaded ...ModelInfo) *Router {
	return &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: "http://n1", Healthy: true, Runtime: runtime,
			LoadedModels: loaded,
		}},
	}
}

func warmValue(r *Router, model string) float64 {
	for _, c := range r.scoreComponents(r.nodes[0], model, "") {
		if c.Name == "warm_model_resident" {
			return c.Raw
		}
	}
	return -1
}

func TestIsModelWarm_BareRequestMatchesTaggedResident(t *testing.T) {
	r := newTaggedRouter("ollama", ModelInfo{Name: "llama3:latest"})
	if !r.isModelWarm(r.nodes[0], "llama3") {
		t.Error("bare request should find the :latest resident warm")
	}
	if r.isModelWarm(r.nodes[0], "llama3:8b") {
		t.Error("a different tag must not count as warm")
	}
	if got := warmValue(r, "llama3"); got != 1.0 {
		t.Errorf("warm factor for bare request = %v, want 1", got)
	}
}

func TestIsModelWarm_TaggedMatchKeepsDigestMismatchCheck(t *testing.T) {
	r := newTaggedRouter("ollama", ModelInfo{Name: "llama3:latest", Digest: "bbb"})
	r.digestMu.Lock()
	r.modelDigests = map[string]string{"llama3:latest": "aaa"}
	r.digestMu.Unlock()
	if r.isModelWarm(r.nodes[0], "llama3") {
		t.Error("digest-conflicting resident must not be warm under an equivalent name")
	}
	if got := warmValue(r, "llama3"); got != 0 {
		t.Errorf("warm factor = %v, want 0 on digest mismatch", got)
	}
}

// ":latest" is an Ollama convention. Other runtimes serve exact ids, so a
// bare request must not be treated as warm on a node reporting "name:latest".
func TestNonOllamaNamesMatchExactlyOnly(t *testing.T) {
	r := newTaggedRouter("vllm", ModelInfo{Name: "llama3:latest"})
	n := r.nodes[0]
	if r.isModelWarm(n, "llama3") {
		t.Error("bare request must not be warm against a :latest resident on a non-Ollama node")
	}
	if r.isEligibleForModel(n, "llama3") {
		t.Error("non-Ollama node reporting only the :latest form must be ineligible for the bare name")
	}
	if got := warmValue(r, "llama3"); got != 0 {
		t.Errorf("warm factor = %v, want 0 for a non-Ollama near-match", got)
	}
	if !r.isEligibleForModel(n, "llama3:latest") {
		t.Error("exact name must stay eligible")
	}
	if r.isEligibleForModel(n, "other") {
		t.Error("non-Ollama node without the model must stay ineligible")
	}
}

func TestInFlightFor_RuntimeScopedEquivalence(t *testing.T) {
	counts := map[string]int32{"m": 1}
	if inFlightFor(counts, "m:latest", "ollama") != 1 || inFlightFor(counts, "m:latest", "") != 1 {
		t.Error("Ollama (or unknown runtime) must treat m and m:latest as one model")
	}
	if inFlightFor(counts, "m:latest", "vllm") != 0 {
		t.Error("non-Ollama runtimes match exact names only")
	}
	if inFlightFor(counts, "m", "vllm") != 1 {
		t.Error("exact name must count on every runtime")
	}
}

func prewarmHitsForResident(t *testing.T, runtime string) int32 {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"model-tod:latest","size":1048576000}]}`))
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: srv.URL, VRAMTotalMB: 8192},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
	for _, d := range []time.Time{
		time.Date(2026, 7, 2, 12, 30, 0, 0, time.Local),
		time.Date(2026, 7, 3, 12, 40, 0, 0, time.Local),
		time.Date(2026, 7, 4, 12, 20, 0, 0, time.Local),
	} {
		r.RecordTransition("model-tod", d)
	}
	hour := r.localNow(time.Date(2026, 7, 2, 12, 30, 0, 0, time.Local)).Hour()
	r.tagsCache[srv.URL] = &TagsCache{
		Models:    []TagModel{{Name: "model-tod", Size: 1000 * 1024 * 1024}},
		FetchedAt: time.Now(),
	}
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-tod:latest"}}
	r.nodes[0].VRAMTotalMB = 8192
	r.nodes[0].Runtime = runtime
	r.runTimeOfDayPrewarm(context.Background(), hour, []*NodeState{r.nodes[0]}, r.warmupCfg, "10m")
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return atomic.LoadInt32(&hits)
}

func TestRunTimeOfDayPrewarm_TaggedResidentIsWarmOnOllama(t *testing.T) {
	if got := prewarmHitsForResident(t, "ollama"); got != 0 {
		t.Fatalf("prewarm reloaded a model already resident as :latest (%d requests)", got)
	}
}

func TestRunTimeOfDayPrewarm_TaggedResidentIsNotWarmOnNonOllama(t *testing.T) {
	if got := prewarmHitsForResident(t, "vllm"); got == 0 {
		t.Fatal("non-Ollama near-match name must not count as already warm")
	}
}
