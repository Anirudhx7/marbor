// Multi-runtime/multi-GPU coverage (Architecture Law 5): this file's
// node/model validation, eviction, and TTFT measurement are all
// runtime-agnostic by construction, not by explicit per-runtime branching -
// node/model discovery goes through the existing Marbor Agent
// models.list/loadedModels endpoints, eviction reuses the same
// UnloadModel/unloadModelViaAgent path handleUnloadModel already uses for
// every runtime, and bench.MeasureChatTTFT talks to the OpenAI-compatible
// /v1/chat/completions surface every runtime (Ollama, vLLM, TGI, llama.cpp,
// MLX) exposes through the marbor proxy. None of it is GPU-vendor-specific
// either - VRAM eviction and TTFT timing don't touch vendor-specific
// telemetry. Covered: all 5 runtimes x all 4 GPU vendors. Deferred: nothing
// - there is no runtime/vendor-specific branch to add here.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/bench"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// benchmarkJobMaxAge mirrors pullJobMaxAge: how long a finished benchmark job
// stays in s.benchJobs after completion, long enough for a late-connecting
// SSE client to still observe the terminal event.
const benchmarkJobMaxAge = 10 * time.Minute

// benchmarkSampleTimeout bounds each individual cold/warm TTFT sample. Cold
// samples on a large model can take a while to load from disk, so this is
// generous like nodePullTimeout rather than tight like nodeUnloadModelTimeout.
var benchmarkSampleTimeout = 5 * time.Minute

// benchmarkKeyTTL is how far in the future the ephemeral benchmark API key's
// expires_at is set - defense in depth only. The key is always explicitly
// deleted (auth.RevokeKey + store.DeleteKey) when the job ends, on every exit
// path (success, error, cancel); this TTL exists purely in case that cleanup
// is ever skipped by a crash mid-job, so an orphaned key can't authenticate
// forever.
const benchmarkKeyTTL = 15 * time.Minute

// benchmarkJob tracks one in-flight or recently-finished hardware benchmark
// run for the progress UI. Mirrors pullJob's shape/lifecycle.
type benchmarkJob struct {
	mu            sync.Mutex
	Node          string              `json:"node"`
	Model         string              `json:"model"`
	N             int                 `json:"n"`
	Phase         string              `json:"phase"` // "evicting" | "cold" | "warm" | "done" | "error" | "cancelled"
	ColdSamplesMs []int64             `json:"cold_samples_ms"`
	WarmSamplesMs []int64             `json:"warm_samples_ms"`
	Error         string              `json:"error,omitempty"`
	Result        *store.BenchmarkRun `json:"result,omitempty"`
	StartedAt     time.Time           `json:"started_at"`
	FinishedAt    time.Time           `json:"finished_at,omitempty"`
	// cancel and keyName are unexported - never appear in the JSON progress
	// payload. keyName is the ephemeral API key's name, used by the job
	// goroutine's deferred cleanup to delete it on every exit path.
	cancel  context.CancelFunc
	keyName string
}

type benchmarkJobSnapshot struct {
	Node          string              `json:"node"`
	Model         string              `json:"model"`
	N             int                 `json:"n"`
	Phase         string              `json:"phase"`
	ColdSamplesMs []int64             `json:"cold_samples_ms"`
	WarmSamplesMs []int64             `json:"warm_samples_ms"`
	Error         string              `json:"error,omitempty"`
	Result        *store.BenchmarkRun `json:"result,omitempty"`
	StartedAt     time.Time           `json:"started_at"`
	FinishedAt    time.Time           `json:"finished_at,omitempty"`
}

func (j *benchmarkJob) snapshot() benchmarkJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	cold := make([]int64, len(j.ColdSamplesMs))
	copy(cold, j.ColdSamplesMs)
	warm := make([]int64, len(j.WarmSamplesMs))
	copy(warm, j.WarmSamplesMs)
	return benchmarkJobSnapshot{
		Node: j.Node, Model: j.Model, N: j.N, Phase: j.Phase,
		ColdSamplesMs: cold, WarmSamplesMs: warm,
		Error: j.Error, Result: j.Result,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

func (j *benchmarkJob) setPhase(phase string) {
	j.mu.Lock()
	j.Phase = phase
	j.mu.Unlock()
}

func (j *benchmarkJob) addColdSample(ms int64) {
	j.mu.Lock()
	j.ColdSamplesMs = append(j.ColdSamplesMs, ms)
	j.mu.Unlock()
}

func (j *benchmarkJob) addWarmSample(ms int64) {
	j.mu.Lock()
	j.WarmSamplesMs = append(j.WarmSamplesMs, ms)
	j.mu.Unlock()
}

func (j *benchmarkJob) finish(phase, errMsg string, result *store.BenchmarkRun) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch j.Phase {
	case "done", "error", "cancelled":
		return
	}
	j.Phase = phase
	j.Error = errMsg
	j.Result = result
	j.FinishedAt = time.Now()
}

// requestCancel marks j cancelled (if not already terminal) and invokes its
// context cancel func. Returns false if the job was already terminal.
func (j *benchmarkJob) requestCancel() bool {
	j.mu.Lock()
	switch j.Phase {
	case "done", "error", "cancelled":
		j.mu.Unlock()
		return false
	}
	j.Phase = "cancelled"
	j.Error = "cancelled by admin"
	j.FinishedAt = time.Now()
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// sweepOldBenchmarkJobs removes finished jobs older than benchmarkJobMaxAge.
// Called opportunistically from handleRunBenchmark, matching
// sweepOldPullJobs's rationale - this map stays tiny in practice.
func (s *Server) sweepOldBenchmarkJobs() {
	s.benchMu.Lock()
	defer s.benchMu.Unlock()
	for id, j := range s.benchJobs {
		snap := j.snapshot()
		if snap.Phase != "evicting" && snap.Phase != "cold" && snap.Phase != "warm" && time.Since(snap.FinishedAt) > benchmarkJobMaxAge {
			delete(s.benchJobs, id)
		}
	}
}

// aggregateSamples returns p50/min/max (ms, as float64) for a non-empty
// sample slice. Callers must not call this with an empty slice.
func aggregateSamples(samples []int64) (p50, min, max float64) {
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		p50 = float64(sorted[n/2])
	} else {
		p50 = float64(sorted[n/2-1]+sorted[n/2]) / 2
	}
	return p50, float64(sorted[0]), float64(sorted[n-1])
}

// handleRunBenchmark starts an in-dashboard hardware benchmark (cold vs warm
// TTFT) against a node+model already known to the marbor. Accepts:
// {"node":"...","model":"...","n":10}. Returns 202 with a job id immediately;
// progress is polled via GET /admin/benchmark/{id}/progress (SSE).
//
// No admin-bypass exists for proxy auth (auth.Middleware requires a real
// client key for every /v1/... request), so this handler auto-provisions a
// scoped, ephemeral API key restricted to the tested model and deletes it
// unconditionally when the job ends - the benchmark measures the real
// client-auth path honestly without asking the operator to paste in a key.
func (s *Server) handleRunBenchmark(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node  string `json:"node"`
		Model string `json:"model"`
		N     int    `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Node == "" || body.Model == "" {
		writeJSONError(w, http.StatusBadRequest, "node and model are required")
		return
	}
	if body.N <= 0 {
		body.N = 10
	}
	if body.N > 50 {
		writeJSONError(w, http.StatusBadRequest, "n must be <= 50")
		return
	}

	if _, ok := s.router.NodeURLs()[body.Node]; !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", body.Node))
		return
	}
	if !nodeIsHealthy(s.router.Nodes(), body.Node) {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("node %q is currently unreachable (down)", body.Node))
		return
	}
	if !s.nodeHasModel(r.Context(), body.Node, body.Model) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("model %q not found on node %q - pull it first", body.Model, body.Node))
		return
	}
	if s.cfg.Proxy.Port <= 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "proxy port is not configured - benchmark requires the marbor proxy to be running")
		return
	}

	s.sweepOldBenchmarkJobs()

	// ctx/cancel and the job struct are cheap to build (no I/O) - creating
	// them before the conflict check, then inserting into s.benchJobs while
	// still holding benchMu, closes the TOCTOU window a separate
	// check-then-insert would leave open: two concurrent POSTs for the same
	// node would otherwise both pass the check before either's insert lands,
	// each evicting/reloading the model out from under the other and
	// corrupting both runs' cold/warm numbers.
	ctx, cancel := context.WithCancel(context.Background())
	jobID := fmt.Sprintf("%d", time.Now().UnixNano())
	job := &benchmarkJob{
		Node:      body.Node,
		Model:     body.Model,
		N:         body.N,
		Phase:     "evicting",
		StartedAt: time.Now(),
		cancel:    cancel,
	}

	s.benchMu.Lock()
	for _, existing := range s.benchJobs {
		snap := existing.snapshot()
		switch snap.Phase {
		case "evicting", "cold", "warm":
			if snap.Node == body.Node {
				s.benchMu.Unlock()
				cancel()
				writeJSONError(w, http.StatusConflict, fmt.Sprintf("a benchmark is already running on node %q", body.Node))
				return
			}
		}
	}
	s.benchJobs[jobID] = job
	s.benchMu.Unlock()

	keyName := fmt.Sprintf("benchmark-%s-%s-%s", body.Node, body.Model, jobID)
	k := config.KeyConfig{
		Name:      keyName,
		Key:       generateAPIKey(keyName),
		Models:    []string{body.Model},
		ExpiresAt: time.Now().Add(benchmarkKeyTTL).Format(time.RFC3339),
	}
	if s.auth != nil {
		s.auth.AddKey(k)
	}
	_ = s.st.UpsertKey(store.KeyRecord{
		Name:      k.Name,
		Key:       k.Key,
		Models:    k.Models,
		Revoked:   false,
		ExpiresAt: k.ExpiresAt,
	})
	job.mu.Lock()
	job.keyName = keyName
	job.mu.Unlock()

	go s.runBenchmarkJob(ctx, job, k.Key)

	s.logSystemChange(r, "run_benchmark", body.Node, fmt.Sprintf("Model: %s, N: %d", body.Model, body.N))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// nodeHasModel checks whether model is known to already be present on node,
// via whichever runtime-agnostic source is available: the Marbor Agent's
// models.list capability if enabled, falling back to the router's live
// LoadedModels view (covers nodes without an agent, or an agent build
// predating models.list) - the same fallback order preflight.sh uses.
func (s *Server) nodeHasModel(ctx context.Context, nodeName, model string) bool {
	for _, n := range s.router.Nodes() {
		if n.Name != nodeName {
			continue
		}
		for _, m := range n.LoadedModels {
			if m.Name == model {
				return true
			}
		}
		break
	}

	agentCfg, agentOK := s.router.MarborAgentSetting(nodeName)
	if !agentOK || !agentCfg.Enabled || !nodeHasAgentCapability(s.router.Nodes(), nodeName, "models.list") {
		return false
	}
	nodeURL, ok := s.router.NodeURLs()[nodeName]
	if !ok {
		return false
	}
	listCtx, cancel := context.WithTimeout(ctx, nodeModelsListTimeout)
	defer cancel()
	models, err := s.listModelsViaAgent(listCtx, nodeURL, agentCfg)
	if err != nil {
		return false
	}
	for _, m := range models {
		if m.Name == model {
			return true
		}
	}
	return false
}

// evictModelForBenchmark unloads model from node, via the Marbor Agent when
// configured or the direct router path otherwise - the same branch
// handleUnloadModel takes, duplicated here as a small standalone helper
// rather than refactoring that hotspot handler to accept a benchmark caller.
func (s *Server) evictModelForBenchmark(ctx context.Context, nodeName, model string) error {
	nodeURL, ok := s.router.NodeURLs()[nodeName]
	if !ok {
		return fmt.Errorf("node %q not found", nodeName)
	}
	if s.router.IsPinned(nodeName, model) {
		return fmt.Errorf("model %q is pinned on node %q - unpin it before benchmarking", model, nodeName)
	}

	agentCfg, useAgent := s.router.ShouldUseAgentForUnload(nodeName)
	if useAgent {
		if !nodeIsHealthy(s.router.Nodes(), nodeName) {
			return fmt.Errorf("node %q is currently unreachable (down)", nodeName)
		}
		unloadCtx, cancel := context.WithTimeout(ctx, nodeUnloadModelTimeout)
		defer cancel()
		ctrl, _ := s.router.NodeControlSetting(nodeName)
		if err := s.unloadModelViaAgent(unloadCtx, nodeURL, agentCfg, model, ctrl); err != nil {
			return err
		}
		s.router.RecordManualUnload(nodeName, model)
		return nil
	}

	found, err := s.router.UnloadModel(ctx, nodeName, model)
	if !found {
		return fmt.Errorf("node %q not found", nodeName)
	}
	return err
}

// runBenchmarkJob is the benchmark job goroutine: evict -> N cold samples
// (evicting before each) -> N warm samples (no evict) -> aggregate ->
// persist -> always clean up the ephemeral key, on every exit path.
func (s *Server) runBenchmarkJob(ctx context.Context, job *benchmarkJob, apiKey string) {
	defer func() {
		if s.auth != nil {
			s.auth.RevokeKey(job.keyName)
		}
		_ = s.st.DeleteKey(job.keyName)
	}()

	target := fmt.Sprintf("http://localhost:%d", s.cfg.Proxy.Port)
	client := &http.Client{Timeout: benchmarkSampleTimeout}

	fail := func(err error) {
		job.finish("error", err.Error(), nil)
	}

	job.setPhase("evicting")
	if err := s.evictModelForBenchmark(ctx, job.Node, job.Model); err != nil {
		fail(fmt.Errorf("initial evict: %w", err))
		return
	}
	time.Sleep(2 * time.Second)

	job.setPhase("cold")
	for i := 0; i < job.N; i++ {
		if ctx.Err() != nil {
			return
		}
		if i > 0 {
			if err := s.evictModelForBenchmark(ctx, job.Node, job.Model); err != nil {
				fail(fmt.Errorf("evict before cold sample %d: %w", i+1, err))
				return
			}
			time.Sleep(2 * time.Second)
		}
		ms, err := bench.MeasureChatTTFT(ctx, client, target, job.Model, apiKey)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(fmt.Errorf("cold sample %d: %w", i+1, err))
			return
		}
		job.addColdSample(ms)
	}

	job.setPhase("warm")
	for i := 0; i < job.N; i++ {
		if ctx.Err() != nil {
			return
		}
		ms, err := bench.MeasureChatTTFT(ctx, client, target, job.Model, apiKey)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(fmt.Errorf("warm sample %d: %w", i+1, err))
			return
		}
		job.addWarmSample(ms)
	}

	snap := job.snapshot()
	if len(snap.ColdSamplesMs) == 0 || len(snap.WarmSamplesMs) == 0 {
		fail(fmt.Errorf("no samples collected"))
		return
	}
	coldP50, coldMin, coldMax := aggregateSamples(snap.ColdSamplesMs)
	warmP50, warmMin, warmMax := aggregateSamples(snap.WarmSamplesMs)
	var speedup float64
	if warmP50 > 0 {
		speedup = coldP50 / warmP50
	}

	run := store.BenchmarkRun{
		Node:      job.Node,
		Model:     job.Model,
		N:         job.N,
		ColdP50Ms: coldP50, ColdMinMs: coldMin, ColdMaxMs: coldMax,
		WarmP50Ms: warmP50, WarmMinMs: warmMin, WarmMaxMs: warmMax,
		SpeedupX:  speedup,
		CreatedAt: time.Now(),
	}
	if err := s.st.InsertBenchmarkRun(run); err != nil {
		fail(fmt.Errorf("save result: %w", err))
		return
	}
	job.finish("done", "", &run)
}

// handleBenchmarkProgress streams live job state via SSE, same 300ms-poll
// shape as handlePullProgress.
func (s *Server) handleBenchmarkProgress(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.benchMu.Lock()
		job, ok := s.benchJobs[jobID]
		s.benchMu.Unlock()
		if !ok {
			fmt.Fprintf(w, "event: not_found\ndata: {}\n\n")
			flusher.Flush()
			return
		}

		snap := job.snapshot()
		data, _ := json.Marshal(snap)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		switch snap.Phase {
		case "done", "error", "cancelled":
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// handleCancelBenchmark aborts an in-flight benchmark job. DELETE
// /admin/benchmark/{id}. The ephemeral key is still cleaned up by
// runBenchmarkJob's deferred cleanup once the goroutine observes the
// cancelled context and returns.
func (s *Server) handleCancelBenchmark(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	s.benchMu.Lock()
	job, ok := s.benchJobs[jobID]
	s.benchMu.Unlock()

	cancelled := false
	if ok {
		cancelled = job.requestCancel()
	}
	if cancelled {
		s.logSystemChange(r, "cancel_benchmark", jobID, "")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "cancelled": cancelled})
}

// handleListBenchmarkRuns returns persisted benchmark history for the
// results table below the setup/progress panel.
func (s *Server) handleListBenchmarkRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.st.ListBenchmarkRuns(50)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to load benchmark history")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"runs": runs})
}
