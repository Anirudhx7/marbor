package router

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	runtimepkg "github.com/Anirudhx7/marbor/internal/runtime"
)

func newReservationRouter(t *testing.T, runtime string) *Router {
	t.Helper()
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "n1", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.nodes[0].Runtime = runtime
	return r
}

func reservationCount(r *Router, node string) int {
	r.evictMu.Lock()
	defer r.evictMu.Unlock()
	return len(r.warmReserved[node])
}

func TestWarmReservation_OllamaBareAndLatestShareOneEntry(t *testing.T) {
	for _, runtime := range []string{"ollama", ""} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			r := newReservationRouter(t, runtime)

			if others := r.reserveWarmBytes("n1", "llama3", 40*mib); others != 0 {
				t.Fatalf("first reserve others = %d, want 0", others)
			}
			if !r.hasActiveWarmReservation("n1", "llama3:latest") {
				t.Error("reservation under bare name not seen by :latest lookup")
			}
			if others := r.reserveWarmBytes("n1", "llama3:latest", 40*mib); others != 0 {
				t.Errorf("re-reserve under :latest counted the bare entry as another model: others = %d", others)
			}
			if got := reservationCount(r, "n1"); got != 1 {
				t.Errorf("entries = %d, want 1", got)
			}
			if others := r.reserveWarmBytes("n1", "mistral", 30*mib); others != 40*mib {
				t.Errorf("distinct model others = %d, want %d", others, 40*mib)
			}
			r.clearWarmReservation("n1", "llama3:latest")
			if r.hasActiveWarmReservation("n1", "llama3") {
				t.Error("clear by :latest left the bare reservation behind")
			}
			if got := r.PendingPrewarmBytes("n1"); got != 30*mib {
				t.Errorf("pending after clear = %d, want %d", got, 30*mib)
			}
		})
	}
}

func TestWarmReservation_NonOllamaStaysExact(t *testing.T) {
	for _, runtime := range []string{"vllm", "llama.cpp", "mlx"} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			r := newReservationRouter(t, runtime)

			r.reserveWarmBytes("n1", "llama3", 40*mib)
			if r.hasActiveWarmReservation("n1", "llama3:latest") {
				t.Error("bare reservation must not be visible under :latest")
			}
			if others := r.reserveWarmBytes("n1", "llama3:latest", 25*mib); others != 40*mib {
				t.Errorf("others = %d, want %d (names are distinct)", others, 40*mib)
			}
			if got := reservationCount(r, "n1"); got != 2 {
				t.Errorf("entries = %d, want 2", got)
			}
			r.clearWarmReservation("n1", "llama3:latest")
			if !r.hasActiveWarmReservation("n1", "llama3") {
				t.Error("clearing :latest must not clear the bare reservation")
			}
		})
	}
}

// A poll that sees the :latest copy resident must clear a reservation made
// under the bare name, through the real poll path.
func TestPollNode_ClearsBareReservationWhenLatestResident(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.nodes[0].mu.Lock()
	r.nodes[0].probe = residentProbe{models: []runtimepkg.LoadedModel{
		{Name: "llama3:latest", SizeVRAMBytes: 40 * mib},
	}}
	r.nodes[0].mu.Unlock()

	r.reserveWarmBytes("n1", "llama3", 40*mib)
	if got := r.PendingPrewarmBytes("n1"); got != 40*mib {
		t.Fatalf("pending before poll = %d, want %d", got, 40*mib)
	}
	r.pollNode(r.nodes[0])
	if got := r.PendingPrewarmBytes("n1"); got != 0 {
		t.Errorf("pending after a poll confirming llama3:latest = %d, want 0", got)
	}
}

// At the per-node cap, re-reserving an existing name's other form is an update,
// not a new entry, so nothing is evicted.
func TestWarmReservation_CapDoesNotEvictForEquivalentForm(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.reserveWarmBytes("n1", "x", 1*mib)
	for i := 1; i < maxWarmReservationsPerNode; i++ {
		r.reserveWarmBytes("n1", fmt.Sprintf("m-%d", i), 1*mib)
	}
	if got := reservationCount(r, "n1"); got != maxWarmReservationsPerNode {
		t.Fatalf("setup: entries = %d, want %d", got, maxWarmReservationsPerNode)
	}
	r.reserveWarmBytes("n1", "x:latest", 2*mib)
	if got := reservationCount(r, "n1"); got != maxWarmReservationsPerNode {
		t.Errorf("entries = %d, want %d unchanged", got, maxWarmReservationsPerNode)
	}
	for _, name := range []string{"x", "x:latest"} {
		if !r.hasActiveWarmReservation("n1", name) {
			t.Errorf("reservation for %q was lost by updating its other form", name)
		}
	}
	r.evictMu.Lock()
	res, ok := r.warmReserved["n1"]["x:latest"]
	r.evictMu.Unlock()
	if !ok || res.bytes != 2*mib {
		t.Errorf("canonical entry = %+v (found %t), want %d bytes", res, ok, 2*mib)
	}
	for i := 1; i < maxWarmReservationsPerNode; i++ {
		if !r.hasActiveWarmReservation("n1", fmt.Sprintf("m-%d", i)) {
			t.Fatalf("m-%d was evicted by an update of an existing name", i)
		}
	}
}

func TestReservationModelKey_OllamaNames(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	for _, tc := range []struct{ in, want string }{
		{"llama3", "llama3:latest"},
		{"llama3:latest", "llama3:latest"},
		{"llama3:8b", "llama3:8b"},
		{"foo@sha256:abc", "foo@sha256:abc"},
		{"", ""},
		{"host:5000/m", "host:5000/m:latest"},
	} {
		if got := r.reservationModelKey("n1", tc.in); got != tc.want {
			t.Errorf("reservationModelKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := canonicalModelName(tc.in); got != tc.want {
			t.Errorf("canonicalModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A node the router does not know has no runtime to judge by, so names stay
// exact there rather than assuming Ollama's shorthand.
func TestWarmReservation_UnknownNodeStaysExact(t *testing.T) {
	r := &Router{}

	r.reserveWarmBytes("ghost", "llama3", 40*mib)
	if r.hasActiveWarmReservation("ghost", "llama3:latest") {
		t.Error("unknown node: bare reservation must not be visible under :latest")
	}
	if !r.hasActiveWarmReservation("ghost", "llama3") {
		t.Error("unknown node: exact lookup must still find the reservation")
	}
}

// The predictive engine records transitions under the name clients typed, while
// a node reports its resident models in the tagged form. A bare-name trigger
// must still be recognised when the node holds the :latest copy.
func TestPredictiveCycle_BareTriggerMatchesLatestResident(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama3:latest", SizeVRAM: 2000 * 1024 * 1024}}
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000

	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("llama3", now)
	r.RecordTransition("model-x", now)
	r.RecordTransition("llama3", now)

	r.RunPredictionCycle(context.Background(), now)

	for _, d := range r.RecentPredictiveDecisions() {
		if d.PredictedModel == "model-x" && d.Node == "node-a" {
			return
		}
	}
	t.Errorf("no decision for model-x on node-a; got %+v", r.RecentPredictiveDecisions())
}

func newPredictiveRouter(runtime, resident string) *Router {
	// The router's fields are set here before any goroutine touches it.
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})
	r.nodes[0].Runtime = runtime
	r.nodes[0].LoadedModels = []ModelInfo{{Name: resident, SizeVRAM: 2000 * 1024 * 1024}}
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000
	return r
}

// Warmup and prediction only target Ollama nodes, so a node on any other
// runtime is skipped by the cycle outright and never produces a decision, while
// an Ollama node with the same history does.
func TestPredictiveCycle_NonOllamaNodeIsNotAPredictionTarget(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	decisionsOn := func(runtime string) []PredictiveDecision {
		r := newPredictiveRouter(runtime, "llama3:latest")
		r.RecordTransition("llama3", now)
		r.RecordTransition("model-x", now)
		r.RunPredictionCycle(context.Background(), now)
		var out []PredictiveDecision
		for _, d := range r.RecentPredictiveDecisions() {
			if d.Node == "node-a" {
				out = append(out, d)
			}
		}
		return out
	}

	if got := decisionsOn("ollama"); len(got) == 0 {
		t.Fatal("control: an Ollama node with the same history should produce a decision")
	}
	for _, runtime := range []string{"vllm", "llama.cpp", "mlx"} {
		t.Run("runtime="+runtime, func(t *testing.T) {
			if got := decisionsOn(runtime); len(got) != 0 {
				t.Errorf("unexpected decisions on a %s node: %+v", runtime, got)
			}
		})
	}
}

// History recorded under both forms of the trigger name is one trigger: the
// counts add up.
func TestPredictiveCycle_TriggerFormsSumTheirCounts(t *testing.T) {
	r := newPredictiveRouter("ollama", "llama3:latest")
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("llama3", now)
	r.RecordTransition("model-x", now)
	r.RecordTransition("llama3:latest", now)
	r.RecordTransition("model-x", now)

	r.RunPredictionCycle(context.Background(), now)

	for _, d := range r.RecentPredictiveDecisions() {
		if d.PredictedModel == "model-x" && d.TriggerModel == "llama3:latest" {
			if d.TransitionCount != 2 {
				t.Errorf("TransitionCount = %d, want 2 (one from each trigger form)", d.TransitionCount)
			}
			return
		}
	}
	t.Errorf("no decision for model-x from llama3:latest; got %+v", r.RecentPredictiveDecisions())
}

// "b" and "b:latest" in the history are one predicted model on Ollama, so a
// trigger yields one decision for it, with the counts summed.
func TestPredictiveCycle_PredictedBareAndLatestAreOneCandidate(t *testing.T) {
	r := newPredictiveRouter("ollama", "w")
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("w", now)
	r.RecordTransition("b", now)
	r.RecordTransition("w", now)
	r.RecordTransition("b:latest", now)

	r.RunPredictionCycle(context.Background(), now)

	var got []PredictiveDecision
	for _, d := range r.RecentPredictiveDecisions() {
		if d.TriggerModel == "w" && ModelNamesEquivalent(d.PredictedModel, "b") {
			got = append(got, d)
		}
	}
	if len(got) != 1 {
		t.Fatalf("decisions for b from w = %d, want 1: %+v", len(got), got)
	}
	if got[0].PredictedModel != "b" {
		t.Errorf("PredictedModel = %q, want the bare form %q", got[0].PredictedModel, "b")
	}
	if got[0].TransitionCount != 2 {
		t.Errorf("TransitionCount = %d, want 2", got[0].TransitionCount)
	}
}

func TestModelNamesMatchOnRuntime_RegistryAndDigestNames(t *testing.T) {
	for _, tc := range []struct {
		runtime, a, b string
		want          bool
	}{
		{"ollama", "host:5000/m", "host:5000/m:latest", true},
		{"", "host:5000/m", "host:5000/m:latest", true},
		{"ollama", "host:5000/m", "host:5000/m:v1", false},
		{"ollama", "host:5000/m:latest", "host:5000/m:v1", false},
		{"ollama", "m@sha256:abc", "m", false},
		{"ollama", "m@sha256:abc", "m:latest", false},
		{"ollama", "m@sha256:abc", "m@sha256:abc", true},
		{"vllm", "host:5000/m", "host:5000/m:latest", false},
	} {
		if got := ModelNamesMatchOnRuntime(tc.runtime, tc.a, tc.b); got != tc.want {
			t.Errorf("ModelNamesMatchOnRuntime(%q, %q, %q) = %t, want %t", tc.runtime, tc.a, tc.b, got, tc.want)
		}
	}
}

// A real reservation made under the other form replaces a placeholder: one
// entry, no longer a placeholder, holding the real bytes.
func TestWarmReservation_PlaceholderUpgradedAcrossForms(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.reserveWarmBytesAs("n1", "m", 5*mib, true)
	r.reserveWarmBytesAs("n1", "m:latest", 40*mib, false)

	if got := reservationCount(r, "n1"); got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
	r.evictMu.Lock()
	res := r.warmReserved["n1"]["m:latest"]
	r.evictMu.Unlock()
	if res.placeholder || res.bytes != 40*mib {
		t.Errorf("entry = %+v, want a non-placeholder of %d bytes", res, 40*mib)
	}
	if got := r.PendingPrewarmReportedBytes("n1"); got != 40*mib {
		t.Errorf("reported pending = %d, want %d", got, 40*mib)
	}
}

// Pending totals count each model once however its name was typed, and the
// reported total leaves placeholders out.
func TestPendingPrewarmBytes_MixedForms(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.reserveWarmBytes("n1", "a:latest", 10*mib)
	r.reserveWarmBytes("n1", "a", 10*mib)
	r.reserveWarmBytesAs("n1", "p", 5*mib, true)
	r.reserveWarmBytesAs("n1", "p:latest", 7*mib, true)

	if got := r.PendingPrewarmBytes("n1"); got != 17*mib {
		t.Errorf("pending = %d, want %d", got, 17*mib)
	}
	if got := r.PendingPrewarmReportedBytes("n1"); got != 10*mib {
		t.Errorf("reported pending = %d, want %d", got, 10*mib)
	}
}

// The key is derived from the runtime of the node at call time, so a
// reservation stored under the Ollama key is not found by a bare-name lookup
// once the node reports another runtime; it ages out with the reservation TTL.
func TestWarmReservation_RuntimeChangeLeavesOldKey(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.reserveWarmBytes("n1", "m", 40*mib)

	r.nodes[0].mu.Lock()
	r.nodes[0].Runtime = "vllm"
	r.nodes[0].mu.Unlock()
	r.clearWarmReservation("n1", "m")

	if got := reservationCount(r, "n1"); got != 1 {
		t.Errorf("entries = %d, want 1 (the old key is left to expire)", got)
	}
}

// A model an operator unloaded stays suppressed under its other name form on
// Ollama, and exactly on every other runtime.
func TestIsWarmupSuppressed_EquivalentForms(t *testing.T) {
	r := newReservationRouter(t, "ollama")
	r.suppressWarmup("n1", "m:latest", "manual")
	for _, name := range []string{"m", "m:latest"} {
		if !r.isWarmupSuppressed("n1", name) {
			t.Errorf("%q should be suppressed after unloading m:latest", name)
		}
	}
	if r.isWarmupSuppressed("n1", "m:8b") {
		t.Error("a different tag must not be suppressed")
	}

	v := newReservationRouter(t, "vllm")
	v.suppressWarmup("n1", "m:latest", "manual")
	if v.isWarmupSuppressed("n1", "m") {
		t.Error("vllm names are exact: bare m must not be suppressed")
	}
	if !v.isWarmupSuppressed("n1", "m:latest") {
		t.Error("exact name should stay suppressed")
	}
}

func seedBothFormSizes(r *Router, url, base string) {
	r.tagsCache[url] = &TagsCache{
		Models: []TagModel{
			{Name: base, Size: 1000 * 1024 * 1024},
			{Name: base + ":latest", Size: 1000 * 1024 * 1024},
		},
		FetchedAt: time.Now(),
	}
}

// Suppressing either form of a predicted model keeps the predictive cycle from
// reloading it, whichever form the history used; with nothing suppressed the
// same history does trigger the warmup.
func TestPredictiveCycle_SuppressedFormCoversBothNames(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		suppress   string
		history    []string
		wantWarmup bool
	}{
		{"control", "", []string{"b", "b:latest"}, true},
		{"latest suppressed, both forms", "b:latest", []string{"b", "b:latest"}, false},
		{"latest suppressed, bare history", "b:latest", []string{"b", "b"}, false},
		{"bare suppressed, latest history", "b", []string{"b:latest", "b:latest"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPredictiveRouter("ollama", "w")
			seedBothFormSizes(r, r.nodes[0].URL, "b")
			if tc.suppress != "" {
				r.suppressWarmup("node-a", tc.suppress, "manual")
			}
			for _, to := range tc.history {
				r.RecordTransition("w", now)
				r.RecordTransition(to, now)
			}
			r.RunPredictionCycle(context.Background(), now)

			found := false
			for _, d := range r.RecentPredictiveDecisions() {
				if d.TriggerModel == "w" && ModelNamesEquivalent(d.PredictedModel, "b") {
					found = true
					if d.WarmupTriggered != tc.wantWarmup {
						t.Errorf("WarmupTriggered = %t, want %t", d.WarmupTriggered, tc.wantWarmup)
					}
				}
			}
			if !found {
				t.Fatal("no decision for b")
			}
		})
	}
}

// prewarmCounter counts predictive prewarm dispatches.
func prewarmCounter(r *Router) *int32 {
	var n int32
	r.prewarmHook = func() { atomic.AddInt32(&n, 1) }
	return &n
}

func timeOfDayRouter(t *testing.T, history ...string) *Router {
	t.Helper()
	r := newPredictiveRouter("ollama", "w")
	seedBothFormSizes(r, r.nodes[0].URL, "b")
	for i, name := range history {
		r.RecordTransition(name, time.Date(2026, 7, 2+i, 15, 30, 0, 0, time.UTC))
	}
	return r
}

func runTimeOfDay(r *Router) {
	r.runTimeOfDayPrewarm(context.Background(), 15, []*NodeState{r.nodes[0]}, r.warmupCfg, "10m")
	time.Sleep(300 * time.Millisecond)
}

// The time-of-day prewarm defers to an operator unload under the other name
// form, and to an in-flight reservation under the other form.
func TestTimeOfDayPrewarm_DefersAcrossForms(t *testing.T) {
	control := timeOfDayRouter(t, "b", "b", "b")
	hits := prewarmCounter(control)
	runTimeOfDay(control)
	if atomic.LoadInt32(hits) == 0 {
		t.Fatal("control: an unsuppressed, unreserved model should be prewarmed")
	}

	suppressed := timeOfDayRouter(t, "b", "b", "b")
	hits = prewarmCounter(suppressed)
	suppressed.suppressWarmup("node-a", "b:latest", "manual")
	runTimeOfDay(suppressed)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("prewarmed a model unloaded under its :latest form %d time(s)", got)
	}

	reserved := timeOfDayRouter(t, "b", "b", "b")
	hits = prewarmCounter(reserved)
	reserved.reserveWarmBytes("node-a", "b:latest", 10*mib)
	runTimeOfDay(reserved)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("prewarmed a model already reserved under its :latest form %d time(s)", got)
	}
}

// A failed predictive prewarm releases a reservation held under the other form.
func TestPrewarmPredicted_FailureClearsOtherFormReservation(t *testing.T) {
	r := newPredictiveRouter("ollama", "w")
	r.nodes[0].URL = "http://127.0.0.1:1"
	r.nodes[0].VRAMTotalMB = 0
	r.reserveWarmBytes("node-a", "m", 10*mib)

	r.prewarmPredicted(context.Background(), r.nodes[0], "m:latest", "10m")

	if r.hasActiveWarmReservation("node-a", "m") {
		t.Error("failed prewarm of m:latest left the reservation made under m")
	}
}

// A failed scheduled warmup releases a reservation held under the other form.
func TestWarmModels_FailureClearsOtherFormReservation(t *testing.T) {
	r := newPredictiveRouter("ollama", "w")
	r.nodes[0].URL = "http://127.0.0.1:1"
	r.nodes[0].VRAMTotalMB = 0
	r.nodes[0].Healthy = true
	r.reserveWarmBytes("node-a", "m", 10*mib)

	if _, err := r.WarmModelsWithWarnings(context.Background(), "node-a", []string{"m:latest"}); err == nil {
		t.Fatal("expected the warmup against a dead node to fail")
	}
	if r.hasActiveWarmReservation("node-a", "m") {
		t.Error("failed warmup of m:latest left the reservation made under m")
	}
}

// A failed keep-warm ping releases a reservation held under the other form.
func TestPingWarmupModels_FailureClearsOtherFormReservation(t *testing.T) {
	r := newPredictiveRouter("ollama", "w")
	r.nodes[0].URL = "http://127.0.0.1:1"
	r.nodes[0].VRAMTotalMB = 0
	r.nodes[0].Healthy = true
	r.warmupCfg = config.WarmupConfig{Enabled: true, KeepAlive: "5m", Models: []config.WarmupEntry{{Model: "m:latest"}}}
	r.reserveWarmBytes("node-a", "m", 10*mib)

	r.pingWarmupModels(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.nodes[0].mu.RLock()
		failed := r.nodes[0].WarmupErrors["m:latest"] != ""
		r.nodes[0].mu.RUnlock()
		if failed || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.hasActiveWarmReservation("node-a", "m") {
		t.Error("failed keep-warm ping of m:latest left the reservation made under m")
	}
}
