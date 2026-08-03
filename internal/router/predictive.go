package router

// predictive.go - Predictive Prewarming Engine.
//
// Implements an in-memory transition history and prediction worker running
// every 5 minutes to proactively warm models based on sequence patterns.
// Also includes a time-of-day pattern worker. No database persistence in this step.

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

type TransitionEntry struct {
	FromModel string
	ToModel   string
	Timestamp time.Time
	HourOfDay int // 0-23
}

type ActivePrediction struct {
	Model     string
	ExpiresAt time.Time
	Met       bool
}

// PredictiveDecision is one entry in the read-only decision-visibility ring
// buffer (last 50), surfaced via GET /api/predictive/decisions.
type PredictiveDecision struct {
	Timestamp       time.Time `json:"timestamp"`
	PredictedModel  string    `json:"predicted_model"`
	TriggerModel    string    `json:"trigger_model"`
	Node            string    `json:"node"`
	WasAlreadyWarm  bool      `json:"was_already_warm"`
	WarmupTriggered bool      `json:"warmup_triggered"`
	TransitionCount int       `json:"transition_count"`
	Hour            int       `json:"hour"`
}

const maxDecisionLogSize = 50

// recordDecision appends d to the capped ring buffer of recent predictive
// decisions. Ephemeral, in-memory only - never persisted to SQLite.
func (r *Router) recordDecision(d PredictiveDecision) {
	r.predictiveMu.Lock()
	defer r.predictiveMu.Unlock()
	r.decisionLog = append(r.decisionLog, d)
	if len(r.decisionLog) > maxDecisionLogSize {
		r.decisionLog = r.decisionLog[len(r.decisionLog)-maxDecisionLogSize:]
	}
}

// RecentPredictiveDecisions returns a copy of the last 50 predictive
// decisions, newest last.
func (r *Router) RecentPredictiveDecisions() []PredictiveDecision {
	r.predictiveMu.Lock()
	defer r.predictiveMu.Unlock()
	out := make([]PredictiveDecision, len(r.decisionLog))
	copy(out, r.decisionLog)
	return out
}

// RecordTransition logs a transition in the ring buffer and, if a store is
// configured, persists it so the predictive engine resumes learned patterns
// instead of rebuilding from zero after a restart.
func (r *Router) RecordTransition(toModel string, now time.Time) {
	if toModel == "" {
		return
	}
	now = r.localNow(now)
	r.mu.RLock()
	st := r.store
	r.mu.RUnlock()

	r.predictiveMu.Lock()
	fromModel := r.lastModelRequested
	r.lastModelRequested = toModel

	entry := TransitionEntry{
		FromModel: fromModel,
		ToModel:   toModel,
		Timestamp: now,
		HourOfDay: now.Hour(),
	}

	r.predictiveHistory = append(r.predictiveHistory, entry)
	if len(r.predictiveHistory) > 500 {
		r.predictiveHistory = r.predictiveHistory[1:]
	}

	// Update active predictions accuracy check
	for i := range r.activePredictions {
		if r.activePredictions[i].Model == toModel && !now.After(r.activePredictions[i].ExpiresAt) {
			r.activePredictions[i].Met = true
		}
	}
	r.predictiveMu.Unlock()

	if st != nil {
		if err := st.AppendPredictiveTransition(fromModel, toModel, now); err != nil {
			log.Printf("predictive: failed to persist transition: %v", err)
		}
	}
}

// SeedPredictiveHistory replaces the in-memory transition ring buffer with
// persisted entries loaded at boot, capped at the same 500-entry limit
// RecordTransition enforces. Called once during startup, before the mesh
// serves traffic.
func (r *Router) SeedPredictiveHistory(entries []TransitionEntry) {
	if len(entries) > 500 {
		entries = entries[len(entries)-500:]
	}
	r.predictiveMu.Lock()
	defer r.predictiveMu.Unlock()
	r.predictiveHistory = append([]TransitionEntry(nil), entries...)
	if len(entries) > 0 {
		r.lastModelRequested = entries[len(entries)-1].ToModel
	}
}

// RunPredictionCycle executes the prediction and accuracy check.
func (r *Router) RunPredictionCycle(ctx context.Context, now time.Time) {
	r.mu.RLock()
	st := r.store
	r.mu.RUnlock()
	if st != nil {
		if val, err := st.GetSetting("predictive_engine_enabled"); err == nil && val == "false" {
			return
		}
	}

	now = r.localNow(now)

	r.predictiveMu.Lock()
	r.cleanExpiredPredictions(now)
	r.predictiveMu.Unlock()

	currentHour := now.Hour()

	// 1. Gather all history transitions in the current hour bucket
	r.predictiveMu.Lock()
	transitions := make(map[string]map[string]int)
	for _, entry := range r.predictiveHistory {
		if entry.HourOfDay == currentHour {
			if _, ok := transitions[entry.FromModel]; !ok {
				transitions[entry.FromModel] = make(map[string]int)
			}
			transitions[entry.FromModel][entry.ToModel]++
		}
	}
	r.predictiveMu.Unlock()

	// 2. Fetch healthy local nodes
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	warmupCfg := r.warmupCfg
	r.mu.RUnlock()

	var healthy []*NodeState
	for _, n := range nodes {
		n.mu.RLock()
		isHealthy := n.Healthy
		isDraining := n.Draining
		prewarmDisabled := n.PrewarmDisabled
		n.mu.RUnlock()
		// A prewarm-disabled node still serves live traffic (unlike Draining) -
		// it is simply excluded from the predictive engine's warmup targets.
		if isHealthy && !isDraining && !prewarmDisabled {
			healthy = append(healthy, n)
		}
	}

	// 3. For each currently-warm model on each healthy node, predict top-3
	keepAlive := effectiveKeepAlive(warmupCfg.KeepAlive, time.Duration(warmupCfg.IntervalMs)*time.Millisecond)

	for _, n := range healthy {
		n.mu.RLock()
		loaded := make(map[string]struct{})
		var warmModels []string
		for _, m := range n.LoadedModels {
			loaded[m.Name] = struct{}{}
			warmModels = append(warmModels, m.Name)
		}
		n.mu.RUnlock()

		for _, W := range warmModels {
			nextCounts, ok := transitions[W]
			if !ok {
				continue
			}

			// Sort by frequency, tiebreak alphabetically
			type modelFreq struct {
				model string
				count int
			}
			var freqs []modelFreq
			for m, count := range nextCounts {
				freqs = append(freqs, modelFreq{m, count})
			}
			sort.Slice(freqs, func(i, j int) bool {
				if freqs[i].count == freqs[j].count {
					return freqs[i].model < freqs[j].model
				}
				return freqs[i].count > freqs[j].count
			})

			// Predict top-3
			limit := 3
			if len(freqs) < limit {
				limit = len(freqs)
			}

			for i := 0; i < limit; i++ {
				P := freqs[i].model
				count := freqs[i].count

				_, wasAlreadyWarm := loaded[P]
				warmupTriggered := false

				// A manual/scheduled unload took P cold on purpose and it's
				// suppressed until an explicit rewarm (see suppressWarmup in
				// eviction.go) - the predictive engine must defer to that
				// operator decision, not silently reload it because it also
				// happens to be a likely-next model. Recorded below as a
				// normal "skipped" decision, same as any other unmet prediction.
				if !wasAlreadyWarm && !r.isWarmupSuppressed(n.Name, P) {
					// Check VRAM headroom
					estSize := r.estimateModelSizeBytes(n.URL, P, true)
					n.mu.RLock()
					freeBytes := (n.VRAMTotalMB - n.VRAMUsedMB) * 1024 * 1024
					n.mu.RUnlock()

					// Headroom is only true if size is known and fits in free VRAM
					if estSize > 0 && freeBytes >= estSize {
						warmupTriggered = true
						go func(targetNode *NodeState, modelToWarm string) {
							r.ensureHeadroom(ctx, targetNode, modelToWarm)
							if err := r.pingNode(ctx, targetNode, modelToWarm, keepAlive); err == nil {
								metrics.WarmupPing(modelToWarm, targetNode.Name, "ok")
							} else {
								metrics.WarmupPing(modelToWarm, targetNode.Name, "error")
							}
						}(n, P)

						// Track prediction accuracy
						r.predictiveMu.Lock()
						r.activePredictions = append(r.activePredictions, ActivePrediction{
							Model:     P,
							ExpiresAt: now.Add(10 * time.Minute),
							Met:       false,
						})
						r.predictiveMu.Unlock()
					}
				}

				// Log prediction decision
				log.Printf("[predictive] decision: predicted_model=%s trigger_model=%s was_already_warm=%t warmup_triggered=%t transition_count=%d hour=%d",
					P, W, wasAlreadyWarm, warmupTriggered, count, currentHour)
				r.recordDecision(PredictiveDecision{
					Timestamp:       now,
					PredictedModel:  P,
					TriggerModel:    W,
					Node:            n.Name,
					WasAlreadyWarm:  wasAlreadyWarm,
					WarmupTriggered: warmupTriggered,
					TransitionCount: count,
					Hour:            currentHour,
				})
			}
		}
	}

	// 4. Run Time-of-day pattern check
	targetTime := now.Add(10 * time.Minute)
	targetHour := targetTime.Hour()

	r.predictiveMu.Lock()
	if r.lastTimeOfDayPrewarmHour != targetHour {
		r.lastTimeOfDayPrewarmHour = targetHour
		r.predictiveMu.Unlock()
		r.runTimeOfDayPrewarm(ctx, targetHour, healthy, warmupCfg, keepAlive)
	} else {
		r.predictiveMu.Unlock()
	}
}

// runTimeOfDayPrewarm prewarms models appearing in targetHour across 3+ days.
func (r *Router) runTimeOfDayPrewarm(ctx context.Context, targetHour int, healthy []*NodeState, warmupCfg config.WarmupConfig, keepAlive string) {
	r.predictiveMu.Lock()
	// Map to count distinct days per model in target hour bucket
	modelDays := make(map[string]map[string]struct{})

	for _, entry := range r.predictiveHistory {
		if entry.HourOfDay == targetHour && entry.ToModel != "" {
			if _, ok := modelDays[entry.ToModel]; !ok {
				modelDays[entry.ToModel] = make(map[string]struct{})
			}
			dayStr := entry.Timestamp.Format("2006-01-02")
			modelDays[entry.ToModel][dayStr] = struct{}{}
		}
	}
	r.predictiveMu.Unlock()

	for model, days := range modelDays {
		distinctDays := len(days)
		if distinctDays >= 3 {
			for _, n := range healthy {
				// Check if warm
				n.mu.RLock()
				loaded := false
				for _, m := range n.LoadedModels {
					if m.Name == model {
						loaded = true
						break
					}
				}
				n.mu.RUnlock()

				// Same deference to an operator's manual/scheduled unload as
				// the transition-based prediction above - a time-of-day
				// pattern must not override an explicit "keep this cold".
				if !loaded && !r.isWarmupSuppressed(n.Name, model) {
					// Check VRAM headroom
					estSize := r.estimateModelSizeBytes(n.URL, model, true)
					n.mu.RLock()
					freeBytes := (n.VRAMTotalMB - n.VRAMUsedMB) * 1024 * 1024
					n.mu.RUnlock()

					if estSize > 0 && freeBytes >= estSize {
						go func(targetNode *NodeState, modelToWarm string) {
							r.ensureHeadroom(ctx, targetNode, modelToWarm)
							if err := r.pingNode(ctx, targetNode, modelToWarm, keepAlive); err == nil {
								metrics.WarmupPing(modelToWarm, targetNode.Name, "ok")
							} else {
								metrics.WarmupPing(modelToWarm, targetNode.Name, "error")
							}
						}(n, model)
						log.Printf("[predictive] time-of-day prewarm triggered for model %q on node %s for hour %d (appeared on %d distinct days)",
							model, n.Name, targetHour, distinctDays)
					}
				}
			}
		}
	}
}

// cleanExpiredPredictions updates metrics and logs hourly accuracy summary.
// Must be called with predictiveMu held.
func (r *Router) cleanExpiredPredictions(now time.Time) {
	var active []ActivePrediction
	for _, p := range r.activePredictions {
		if now.After(p.ExpiresAt) {
			r.predictionsMadeTotal++
			if p.Met {
				r.predictionsMetTotal++
			}
		} else {
			active = append(active, p)
		}
	}
	r.activePredictions = active

	if r.predictionsMadeTotal > 0 {
		ratio := float64(r.predictionsMetTotal) / float64(r.predictionsMadeTotal)
		metrics.PredictionAccuracyRatio(ratio)

		if now.Sub(r.lastAccuracyLogAt) >= 1*time.Hour {
			log.Printf("[predictive] accuracy summary: total_predictions=%d met_predictions=%d ratio=%.4f",
				r.predictionsMadeTotal, r.predictionsMetTotal, ratio)
			r.lastAccuracyLogAt = now
		}
	}
}
