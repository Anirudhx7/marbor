package router

import (
	"context"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
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
		r := newReservationRouter(t, runtime)

		if others := r.reserveWarmBytes("n1", "llama3", 40*mib); others != 0 {
			t.Fatalf("runtime %q: first reserve others = %d, want 0", runtime, others)
		}
		if !r.hasActiveWarmReservation("n1", "llama3:latest") {
			t.Errorf("runtime %q: reservation under bare name not seen by :latest lookup", runtime)
		}
		if others := r.reserveWarmBytes("n1", "llama3:latest", 40*mib); others != 0 {
			t.Errorf("runtime %q: re-reserve under :latest counted the bare entry as another model: others = %d", runtime, others)
		}
		if got := reservationCount(r, "n1"); got != 1 {
			t.Errorf("runtime %q: entries = %d, want 1", runtime, got)
		}
		if others := r.reserveWarmBytes("n1", "mistral", 30*mib); others != 40*mib {
			t.Errorf("runtime %q: distinct model others = %d, want %d", runtime, others, 40*mib)
		}
		r.clearWarmReservation("n1", "llama3:latest")
		if r.hasActiveWarmReservation("n1", "llama3") {
			t.Errorf("runtime %q: clear by :latest left the bare reservation behind", runtime)
		}
		if got := r.PendingPrewarmBytes("n1"); got != 30*mib {
			t.Errorf("runtime %q: pending after clear = %d, want %d", runtime, got, 30*mib)
		}
	}
}

func TestWarmReservation_NonOllamaStaysExact(t *testing.T) {
	r := newReservationRouter(t, "vllm")

	r.reserveWarmBytes("n1", "llama3", 40*mib)
	if r.hasActiveWarmReservation("n1", "llama3:latest") {
		t.Error("vllm: bare reservation must not be visible under :latest")
	}
	if others := r.reserveWarmBytes("n1", "llama3:latest", 25*mib); others != 40*mib {
		t.Errorf("vllm: others = %d, want %d (names are distinct)", others, 40*mib)
	}
	if got := reservationCount(r, "n1"); got != 2 {
		t.Errorf("vllm: entries = %d, want 2", got)
	}
	r.clearWarmReservation("n1", "llama3:latest")
	if !r.hasActiveWarmReservation("n1", "llama3") {
		t.Error("vllm: clearing :latest must not clear the bare reservation")
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
