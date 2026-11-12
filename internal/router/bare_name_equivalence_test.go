package router

import (
	"context"
	"fmt"
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
		runtime := runtime
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
		runtime := runtime
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

// A non-Ollama node serves exact ids, so a bare-name history must not trigger
// a prediction for a resident ":latest" id.
func TestPredictiveCycle_NonOllamaBareHistoryDoesNotTrigger(t *testing.T) {
	for _, runtime := range []string{"vllm", "llama.cpp", "mlx"} {
		runtime := runtime
		t.Run("runtime="+runtime, func(t *testing.T) {
			r := newPredictiveRouter(runtime, "llama3:latest")
			now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
			r.RecordTransition("llama3", now)
			r.RecordTransition("model-x", now)

			r.RunPredictionCycle(context.Background(), now)

			for _, d := range r.RecentPredictiveDecisions() {
				if d.Node == "node-a" {
					t.Errorf("unexpected decision on a %s node: %+v", runtime, d)
				}
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
	if got[0].TransitionCount != 2 {
		t.Errorf("TransitionCount = %d, want 2", got[0].TransitionCount)
	}
}
