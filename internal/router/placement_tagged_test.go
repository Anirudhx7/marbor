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

func TestIsEligibleForModel_NonOllamaBareRequestTaggedResident(t *testing.T) {
	r := newTaggedRouter("vllm", ModelInfo{Name: "llama3:latest"})
	if !r.isEligibleForModel(r.nodes[0], "llama3") {
		t.Error("non-Ollama node reporting the :latest form must stay eligible for the bare name")
	}
	if r.isEligibleForModel(r.nodes[0], "other") {
		t.Error("non-Ollama node without the model must stay ineligible")
	}
}

func TestRunTimeOfDayPrewarm_TaggedResidentIsWarm(t *testing.T) {
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
	r.runTimeOfDayPrewarm(context.Background(), hour, []*NodeState{r.nodes[0]}, r.warmupCfg, "10m")
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("prewarm reloaded a model already resident as :latest (%d requests)", got)
	}
}
