package router

import (
	"context"
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

func TestCloudOnly_Match(t *testing.T) {
	cases := []struct {
		name  string
		list  []string
		query string
		want  bool
	}{
		{"empty list", nil, "llama3", false},
		{"exact", []string{"llama3"}, "llama3", true},
		{"exact no match", []string{"llama3"}, "mistral", false},
		{"bare entry catches latest", []string{"llama3"}, "llama3:latest", true},
		{"latest entry catches bare", []string{"llama3:latest"}, "llama3", true},
		{"other tag not caught", []string{"llama3"}, "llama3:8b", false},
		{"tagged entry exact", []string{"llama3:8b"}, "llama3:8b", true},
		{"tagged entry other tag", []string{"llama3:8b"}, "llama3:70b", false},
		{"registry port name", []string{"host:5000/ns/model"}, "host:5000/ns/model:latest", true},
		{"digest only exact", []string{"llama3@sha256:abc"}, "llama3@sha256:abc", true},
		{"digest not equivalent to bare", []string{"llama3"}, "llama3@sha256:abc", false},
		{"library spelling not caught", []string{"llama3"}, "library/llama3", false},
		{"vllm style over-catch", []string{"foo"}, "foo:latest", true},
		{"prefix", []string{"qwen*"}, "qwen2.5:7b", true},
		{"prefix no match", []string{"qwen*"}, "llama3", false},
		{"prefix empty rest", []string{"qwen*"}, "qwen", true},
		{"case exact", []string{"llama3"}, "LLAMA3", true},
		{"case entry upper", []string{"Llama3"}, "llama3:Latest", true},
		{"case prefix", []string{"QWEN*"}, "qwen2", true},
		{"case prefix query", []string{"qwen*"}, "Qwen2:7B", true},
		{"empty name", []string{"llama3", "qwen*"}, "", false},
		{"bare star entry ignored", []string{"*"}, "anything", false},
		{"empty entry ignored", []string{""}, "anything", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(config.RoutingConfig{CloudOnlyModels: tc.list}, nil, nil)
			if got := r.IsCloudOnly(tc.query); got != tc.want {
				t.Fatalf("IsCloudOnly(%q) with %v = %v, want %v", tc.query, tc.list, got, tc.want)
			}
		})
	}

	r := New(config.RoutingConfig{CloudOnlyModels: []string{"b-model"}}, nil, nil)
	if !r.IsCloudOnly("a-model", "b-model") {
		t.Fatal("any of several names matching must count")
	}
	if r.IsCloudOnly("") || r.IsCloudOnly("", "") {
		t.Fatal("an empty or absent model name must never match")
	}
}

func TestCloudOnly_LabelIgnoresPrefixEntries(t *testing.T) {
	r := New(config.RoutingConfig{CloudOnlyModels: []string{"Llama3", "qwen*"}}, nil, nil)
	for _, name := range []string{"llama3:latest", "LLAMA3", "Llama3:Latest"} {
		if got := r.CloudOnlyLabel(name); got != "llama3" {
			t.Fatalf("CloudOnlyLabel(%q) = %q, want the lower-cased entry llama3", name, got)
		}
	}
	if got := r.CloudOnlyLabel("qwen2"); got != "" {
		t.Fatalf("a name caught only by a prefix entry must have no label, got %q", got)
	}
	if got := r.CloudOnlyLabel(""); got != "" {
		t.Fatalf("empty name must have no label, got %q", got)
	}
}

func TestCloudOnly_SetDropsEmptyAndBareStarFromReadBack(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.SetCloudOnlyModels([]string{"a", "", "*", "b*"})
	got := r.CloudOnlyModels()
	if len(got) != 2 || got[0] != "a" || got[1] != "b*" {
		t.Fatalf("CloudOnlyModels() = %v, want [a b*]: the read-back must match what is enforced", got)
	}
	if r.IsCloudOnly("anything") {
		t.Fatal("a bare * must not act as a wildcard")
	}
}

func TestCloudOnly_SetCopiesAndReadsBack(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	in := []string{"a", "b*"}
	r.SetCloudOnlyModels(in)
	in[0] = "mutated"
	if !r.IsCloudOnly("a") || r.IsCloudOnly("mutated") {
		t.Fatal("SetCloudOnlyModels must copy its input")
	}
	got := r.CloudOnlyModels()
	if len(got) != 2 || got[0] != "a" || got[1] != "b*" {
		t.Fatalf("CloudOnlyModels() = %v, want [a b*]", got)
	}
	got[0] = "mutated"
	if r.CloudOnlyModels()[0] != "a" {
		t.Fatal("CloudOnlyModels must return a copy")
	}
	r.SetCloudOnlyModels(nil)
	if r.IsCloudOnly("a") || len(r.CloudOnlyModels()) != 0 {
		t.Fatal("nil must clear the list")
	}
	if r.CloudOnlyModels() == nil {
		t.Fatal("CloudOnlyModels must never return nil")
	}
}

func TestCloudOnly_SwapRace(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.IsCloudOnly("alpha", "beta:latest")
					r.CloudOnlyLabel("beta")
					_ = r.CloudOnlyModels()
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		if i%2 == 0 {
			r.SetCloudOnlyModels([]string{"alpha", "gam*"})
		} else {
			r.SetCloudOnlyModels([]string{"beta"})
		}
	}
	close(stop)
	wg.Wait()
}

func TestPrewarm_SkipsCloudOnly(t *testing.T) {
	cycleTime := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	setup := func(listed bool) *Router {
		cfg := config.RoutingConfig{Strategy: "warm-first"}
		if listed {
			cfg.CloudOnlyModels = []string{"model-x"}
		}
		r := New(cfg, []config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
		}, nil)
		r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
		r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * 1024 * 1024}}
		r.nodes[0].VRAMTotalMB = 16384
		r.nodes[0].VRAMUsedMB = 2000
		r.RecordTransition("model-w", cycleTime)
		r.RecordTransition("model-x", cycleTime)
		r.RecordTransition("model-w", cycleTime)
		r.tagsCache["http://localhost:11434"] = &TagsCache{
			Models:    []TagModel{{Name: "model-x", Size: 4000 * 1024 * 1024}},
			FetchedAt: time.Now(),
		}
		return r
	}
	triggered := func(r *Router) (found, trig bool) {
		r.RunPredictionCycle(context.Background(), cycleTime)
		for _, d := range r.RecentPredictiveDecisions() {
			if d.PredictedModel == "model-x" && d.Node == "node-a" {
				found = true
				trig = trig || d.WarmupTriggered
			}
		}
		return found, trig
	}

	found, trig := triggered(setup(false))
	if !found || !trig {
		t.Fatalf("control: an unlisted model must still be pre-warmed (found=%v triggered=%v)", found, trig)
	}
	r := setup(true)
	found, trig = triggered(r)
	if !found {
		t.Fatal("history prediction: expected a recorded decision for the listed model")
	}
	if trig {
		t.Fatal("history prediction triggered a warmup for a cloud-only model")
	}
	r.predictiveMu.Lock()
	n := len(r.activePredictions)
	r.predictiveMu.Unlock()
	if n != 0 {
		t.Fatalf("a skipped model must not leave %d prediction(s) to be scored", n)
	}
	if r.hasActiveWarmReservation("node-a", "model-x") {
		t.Fatal("a skipped model must not hold a warm reservation")
	}
}

func TestPrewarm_SkipsCloudOnly_TimeOfDay(t *testing.T) {
	run := func(listed bool) int32 {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Write([]byte(`{"done":true}`))
		}))
		defer srv.Close()
		cfg := config.RoutingConfig{Strategy: "warm-first"}
		if listed {
			cfg.CloudOnlyModels = []string{"model-tod"}
		}
		r := New(cfg, []config.NodeConfig{{Name: "node-a", URL: srv.URL, VRAMTotalMB: 8192}}, nil)
		r.nodes[0].Healthy = true
		r.nodes[0].Runtime = "ollama"
		r.nodes[0].VRAMTotalMB = 8192
		r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
		r.RecordTransition("model-tod", time.Date(2026, 7, 2, 15, 30, 0, 0, time.UTC))
		r.RecordTransition("model-tod", time.Date(2026, 7, 3, 15, 45, 0, 0, time.UTC))
		r.RecordTransition("model-tod", time.Date(2026, 7, 4, 15, 15, 0, 0, time.UTC))
		r.tagsCache[srv.URL] = &TagsCache{
			Models:    []TagModel{{Name: "model-tod", Size: 1000 * 1024 * 1024}},
			FetchedAt: time.Now(),
		}
		r.runTimeOfDayPrewarm(context.Background(), 15, []*NodeState{r.nodes[0]}, r.warmupCfg, "10m")
		time.Sleep(300 * time.Millisecond)
		return atomic.LoadInt32(&hits)
	}
	if got := run(false); got == 0 {
		t.Fatal("control: an unlisted time-of-day model must still be pre-warmed")
	}
	if got := run(true); got != 0 {
		t.Fatalf("time-of-day prewarm pinged a cloud-only model %d time(s); want 0", got)
	}
}

// loadCounter is a fake runtime that counts load requests per model name.
func loadCounter(listed string, listedHits, otherHits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		if strings.Contains(string(raw), listed) {
			atomic.AddInt32(listedHits, 1)
		} else {
			atomic.AddInt32(otherHits, 1)
		}
		w.Write([]byte(`{"done":true}`))
	}))
}

func TestWarmer_SkipsCloudOnly(t *testing.T) {
	var listedHits, otherHits int32
	srv := loadCounter("paid-model", &listedHits, &otherHits)
	defer srv.Close()

	r := &Router{
		client: &http.Client{Timeout: 5 * time.Second},
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true, Runtime: "ollama"}},
		warmupCfg: config.WarmupConfig{
			Enabled:   true,
			KeepAlive: "5m",
			Models:    []config.WarmupEntry{{Model: "paid-model"}, {Model: "local-model"}},
		},
	}
	r.SetCloudOnlyModels([]string{"paid-model"})
	r.pingWarmupModels(context.Background())
	time.Sleep(400 * time.Millisecond)

	if got := atomic.LoadInt32(&listedHits); got != 0 {
		t.Fatalf("keep-warm loaded a cloud-only model %d time(s); want 0", got)
	}
	if got := atomic.LoadInt32(&otherHits); got == 0 {
		t.Fatal("keep-warm must still load an unlisted model")
	}
}

func TestScheduler_SkipsCloudOnly(t *testing.T) {
	var listedHits, otherHits int32
	srv := loadCounter("paid-model", &listedHits, &otherHits)
	defer srv.Close()

	r := &Router{
		client:         &http.Client{Timeout: 5 * time.Second},
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true, Runtime: "ollama"}},
		schedLastFired: map[string]string{},
	}
	r.SetCloudOnlyModels([]string{"paid-model"})
	if err := r.WarmModels(context.Background(), "n1", []string{"paid-model", "local-model"}); err != nil {
		t.Fatalf("WarmModels: %v", err)
	}
	if got := atomic.LoadInt32(&listedHits); got != 0 {
		t.Fatalf("scheduled warmup loaded a cloud-only model %d time(s); want 0", got)
	}
	if got := atomic.LoadInt32(&otherHits); got == 0 {
		t.Fatal("scheduled warmup must still load an unlisted model")
	}
}

func TestScheduler_SkippedCloudOnlyModelIsSurfacedInRunStatus(t *testing.T) {
	var listedHits, otherHits int32
	srv := loadCounter("paid-model", &listedHits, &otherHits)
	defer srv.Close()

	r := &Router{
		client:         &http.Client{Timeout: 5 * time.Second},
		nodes:          []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true, Runtime: "ollama"}},
		schedLastFired: map[string]string{},
		schedules:      []Schedule{{ID: "s1", Action: "warmup", Node: "n1", Models: []string{"paid-model"}, Enabled: true}},
	}
	r.SetCloudOnlyModels([]string{"paid-model"})
	r.fireSchedule(context.Background(), r.schedules[0])
	got := r.schedules[0]
	if got.LastStatus != "ok" {
		t.Fatalf("LastStatus = %q, want ok: a skip is not a failure", got.LastStatus)
	}
	if !strings.Contains(got.LastError, "paid-model") || !strings.Contains(got.LastError, "cloud-only") {
		t.Fatalf("run note = %q, want it to name the skipped cloud-only model", got.LastError)
	}
}

// A direct client request for a listed model is unaffected by the automatic
// loaders' skip: the router itself still places it. The cloud-only decision
// belongs to the proxy, before routing, and not to the router's placement.
func TestCloudOnly_AutomaticLoaderSkipDoesNotAffectRouting(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", CloudOnlyModels: []string{"model-a"}}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.nodes[0].Healthy = true
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-a", SizeVRAM: 1000 * 1024 * 1024}}
	if !r.skipCloudOnlyPrewarm("node-a", "model-a") {
		t.Fatal("the automatic loaders must skip a listed model")
	}
	n, _, _ := r.Route("model-a", "", "")
	if n == nil || n.Name != "node-a" {
		t.Fatalf("Route for a listed model = %v, want node-a (placement is unchanged by the loaders' skip)", n)
	}
}

func TestCloudOnly_UnreadableFailsClosedUntilListSet(t *testing.T) {
	r := New(config.RoutingConfig{CloudOnlyListUnreadable: true}, nil, nil)
	if !r.CloudOnlyUnreadable() {
		t.Fatal("New must carry the unreadable state from the config")
	}
	if !r.IsCloudOnly("anything") || !r.IsCloudOnly("", "x") {
		t.Error("an unreadable list must treat every named model as listed")
	}
	if r.IsCloudOnly("") || r.IsCloudOnly() {
		t.Error("an empty name must still never match")
	}
	r.SetCloudOnlyModels([]string{"only"})
	if r.CloudOnlyUnreadable() || r.IsCloudOnly("anything") || !r.IsCloudOnly("only") {
		t.Error("a valid list must replace the unreadable state")
	}
	r.SetCloudOnlyUnreadable()
	r.SetCloudOnlyModels(nil)
	if r.CloudOnlyUnreadable() || r.IsCloudOnly("anything") {
		t.Error("clearing the list must also lift the unreadable state")
	}
}
