package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// mockStore implementing Store just to return empty tags for headroom estimation in tests.
type mockTagsStore struct{}

func (s *mockTagsStore) EstimateSize(nodeURL, model string) int64 {
	return 4000 * 1024 * 1024 // 4GB
}

func TestPredictivePrewarming(t *testing.T) {
	newTestRouter := func(nodesCfg []config.NodeConfig) *Router {
		r := New(config.RoutingConfig{
			Strategy: "warm-first",
		}, nodesCfg, nil)
		r.SetWarmupConfig(config.WarmupConfig{
			Enabled:    true,
			IntervalMs: 300000,
		})
		return r
	}

	t.Run("Transition logging & Ring buffer cap", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 8192},
		})

		now := time.Now()
		// Log 510 transitions
		for i := 0; i < 510; i++ {
			r.RecordTransition("model-x", now)
		}

		r.predictiveMu.Lock()
		historyLen := len(r.predictiveHistory)
		r.predictiveMu.Unlock()

		if historyLen != 500 {
			t.Errorf("expected predictive history length to cap at 500, got %d", historyLen)
		}
	})

	t.Run("Prediction cycle triggers prewarm with headroom", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384}, // 16GB
		})

		// Make node-a warm for trigger model-w
		r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * 1024 * 1024}}
		r.nodes[0].VRAMTotalMB = 16384
		r.nodes[0].VRAMUsedMB = 2000

		// Mock tags for size estimation: make predicted cold model-x need 4GB, model-y need 4GB, model-z need 4GB, model-other need 4GB.
		// Since node-a has 16GB total and 2GB used, free VRAM is 14GB.
		// W is warm.
		// Set transitions for hour 14:
		// model-w -> model-x (10 times)
		// model-w -> model-y (5 times)
		// model-w -> model-z (3 times)
		// model-w -> model-other (1 time)
		now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)

		r.RecordTransition("model-w", now)
		for i := 0; i < 10; i++ {
			r.RecordTransition("model-x", now)
			r.RecordTransition("model-w", now)
		}
		for i := 0; i < 5; i++ {
			r.RecordTransition("model-y", now)
			r.RecordTransition("model-w", now)
		}
		for i := 0; i < 3; i++ {
			r.RecordTransition("model-z", now)
			r.RecordTransition("model-w", now)
		}
		r.RecordTransition("model-other", now)

		// Set up mock tag caching on the router
		r.tagsCache["http://localhost:11434"] = &TagsCache{
			Models: []TagModel{
				{Name: "model-x", Size: 4000 * 1024 * 1024},
				{Name: "model-y", Size: 4000 * 1024 * 1024},
				{Name: "model-z", Size: 4000 * 1024 * 1024},
			},
			FetchedAt: time.Now(),
		}

		// Run prediction cycle
		ctx := context.Background()
		r.RunPredictionCycle(ctx, now)

		// Verify predictions were added to activePredictions
		r.predictiveMu.Lock()
		activeCount := len(r.activePredictions)
		predictions := append([]ActivePrediction(nil), r.activePredictions...)
		r.predictiveMu.Unlock()

		// Top 3 next models are model-x, model-y, model-z.
		// All 3 should fit in headroom (4GB * 3 = 12GB < 14GB free).
		// So all 3 should be predicted and warmups triggered.
		if activeCount < 3 {
			t.Errorf("expected at least 3 active predictions, got %d (list: %+v)", activeCount, predictions)
		}
	})

	t.Run("Time-of-day prewarming across 3+ days", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 8192},
		})
		r.nodes[0].VRAMTotalMB = 8192

		// Log request for model-tod in hour 15 across 3 distinct days:
		// Day 1 (July 2), Day 2 (July 3), Day 3 (July 4)
		day1 := time.Date(2026, 7, 2, 15, 30, 0, 0, time.UTC)
		day2 := time.Date(2026, 7, 3, 15, 45, 0, 0, time.UTC)
		day3 := time.Date(2026, 7, 4, 15, 15, 0, 0, time.UTC)

		r.RecordTransition("model-tod", day1)
		r.RecordTransition("model-tod", day2)
		r.RecordTransition("model-tod", day3)

		// Set up mock tag cache
		r.tagsCache["http://localhost:11434"] = &TagsCache{
			Models: []TagModel{
				{Name: "model-tod", Size: 1000 * 1024 * 1024},
			},
			FetchedAt: time.Now(),
		}

		// Run prediction cycle at 14:50 (which is 10 minutes before target hour 15 starts)
		runTime := time.Date(2026, 7, 5, 14, 50, 0, 0, time.UTC)
		ctx := context.Background()

		// Let's hook a channel or verify execution: we can check if it logs or triggers warmup
		r.RunPredictionCycle(ctx, runTime)

		// Clean up and verify lastTimeOfDayPrewarmHour was set to 15
		r.predictiveMu.Lock()
		targetHour := r.lastTimeOfDayPrewarmHour
		r.predictiveMu.Unlock()

		if targetHour != 15 {
			t.Errorf("expected lastTimeOfDayPrewarmHour to be 15, got %d", targetHour)
		}
	})

	t.Run("Accuracy tracking logic", func(t *testing.T) {
		r := newTestRouter([]config.NodeConfig{
			{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 8192},
		})

		now := time.Now()

		r.predictiveMu.Lock()
		r.activePredictions = append(r.activePredictions, ActivePrediction{
			Model:     "model-x",
			ExpiresAt: now.Add(10 * time.Minute),
			Met:       false,
		})
		r.predictiveMu.Unlock()

		// Model is actually requested (met)
		r.RecordTransition("model-x", now.Add(2*time.Minute))

		// Clean expired predictions at expiration time + 1s
		r.predictiveMu.Lock()
		r.cleanExpiredPredictions(now.Add(11 * time.Minute))
		made := r.predictionsMadeTotal
		met := r.predictionsMetTotal
		r.predictiveMu.Unlock()

		if made != 1 {
			t.Errorf("expected 1 prediction made, got %d", made)
		}
		if met != 1 {
			t.Errorf("expected 1 prediction met, got %d", met)
		}
	})
}

// TestRecentPredictiveDecisions_RecordsAndCaps verifies that RunPredictionCycle
// records a decision per evaluated (model, node) pair into the read-only
// visibility ring buffer, and that the buffer stays capped at 50 entries.
func TestRecentPredictiveDecisions_RecordsAndCaps(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})

	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * 1024 * 1024}}
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000

	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("model-w", now)
	r.RecordTransition("model-x", now)
	r.RecordTransition("model-w", now)

	ctx := context.Background()
	r.RunPredictionCycle(ctx, now)

	decisions := r.RecentPredictiveDecisions()
	if len(decisions) == 0 {
		t.Fatal("expected at least one recorded predictive decision after a prediction cycle")
	}
	found := false
	for _, d := range decisions {
		if d.PredictedModel == "model-x" && d.TriggerModel == "model-w" && d.Node == "node-a" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a decision for predicted=model-x trigger=model-w node=node-a, got %+v", decisions)
	}

	// Filling the buffer well past its cap must never grow it past 50.
	for i := 0; i < 100; i++ {
		r.recordDecision(PredictiveDecision{PredictedModel: "filler", TriggerModel: "filler"})
	}
	if got := len(r.RecentPredictiveDecisions()); got != maxDecisionLogSize {
		t.Errorf("decision log length = %d, want capped at %d", got, maxDecisionLogSize)
	}
}

// TestRunPredictionCycleSkipsSuppressedModel reproduces the reported gap: a
// model an operator manually/scheduled-unloaded (suppressed until an
// explicit rewarm, see suppressWarmup in eviction.go) could get silently
// reloaded by the predictive engine if it also happened to be a likely-next
// model in the transition history - overriding the operator's decision the
// exact same way the periodic keep-warm pinger used to before it checked
// isWarmupSuppressed. The predicted model must not be pinged, and its
// decision-log entry must show warmup_triggered=false (surfaced as "skipped"
// in the Predictions tab).
func TestRunPredictionCycleSkipsSuppressedModel(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})

	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * 1024 * 1024}}
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000

	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("model-w", now)
	r.RecordTransition("model-x", now)
	r.RecordTransition("model-w", now)

	r.tagsCache["http://localhost:11434"] = &TagsCache{
		Models:    []TagModel{{Name: "model-x", Size: 4000 * 1024 * 1024}},
		FetchedAt: time.Now(),
	}

	// Operator manually unloaded model-x - it must stay cold, not get reloaded
	// by the predictive engine on the very next cycle.
	r.suppressWarmup("node-a", "model-x", "manual")

	ctx := context.Background()
	r.RunPredictionCycle(ctx, now)

	decisions := r.RecentPredictiveDecisions()
	found := false
	for _, d := range decisions {
		if d.PredictedModel == "model-x" && d.Node == "node-a" {
			found = true
			if d.WarmupTriggered {
				t.Error("predictive engine triggered warmup for a suppressed model; want skipped")
			}
		}
	}
	if !found {
		t.Fatalf("expected a decision for predicted=model-x node=node-a, got %+v", decisions)
	}
}

// TestRunTimeOfDayPrewarmSkipsSuppressedModel is the same regression for the
// time-of-day prewarm path (runTimeOfDayPrewarm), the predictive engine's
// other direct pingNode call site.
func TestRunTimeOfDayPrewarmSkipsSuppressedModel(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: srv.URL, VRAMTotalMB: 8192},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})

	day1 := time.Date(2026, 7, 2, 15, 30, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 3, 15, 45, 0, 0, time.UTC)
	day3 := time.Date(2026, 7, 4, 15, 15, 0, 0, time.UTC)
	r.RecordTransition("model-tod", day1)
	r.RecordTransition("model-tod", day2)
	r.RecordTransition("model-tod", day3)

	r.tagsCache[srv.URL] = &TagsCache{
		Models:    []TagModel{{Name: "model-tod", Size: 1000 * 1024 * 1024}},
		FetchedAt: time.Now(),
	}

	r.suppressWarmup("node-a", "model-tod", "scheduled")

	healthy := []*NodeState{r.nodes[0]}
	r.runTimeOfDayPrewarm(context.Background(), 15, healthy, r.warmupCfg, "10m")
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("time-of-day prewarm pinged a suppressed model %d time(s); want 0", got)
	}
}

// Ensure thread safety of predictive prewarming under concurrent load
func TestPredictiveConcurrency(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 8192},
	}, nil)

	var wg sync.WaitGroup
	ctx := context.Background()
	now := time.Now()

	// Spin up goroutines concurrently writing transitions and running predictions
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r.RecordTransition("model-a", now)
			r.RecordTransition("model-b", now)
			r.RunPredictionCycle(ctx, now)
		}(i)
	}

	wg.Wait()
}
