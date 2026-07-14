package router

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// Schedule is a recurring, time-of-day action against a node. It powers the
// operator use case "warm the model before the 9am shift" and "drain node X at
// night". Times are server-local HH:MM; Days uses 0=Sunday..6=Saturday, and an
// empty Days means every day.
type Schedule struct {
	ID      string   `json:"id"`
	Action  string   `json:"action"` // "warmup" | "unload" | "drain" | "undrain"
	Node    string   `json:"node"`
	Models  []string `json:"models,omitempty"` // used by the "warmup" and "unload" actions
	At      string   `json:"at"`               // "HH:MM", server-local time
	Days    []int    `json:"days,omitempty"`   // 0=Sun..6=Sat; empty = every day
	Enabled bool     `json:"enabled"`
}

// ValidScheduleAction reports whether a is a supported schedule action.
func ValidScheduleAction(a string) bool {
	return a == "warmup" || a == "unload" || a == "drain" || a == "undrain"
}

// SetSchedules replaces the in-memory schedule set (persistence is the caller's
// job via the KV store). Safe for concurrent use.
func (r *Router) SetSchedules(s []Schedule) {
	r.mu.Lock()
	r.schedules = append([]Schedule(nil), s...)
	r.mu.Unlock()
}

// Schedules returns a copy of the current schedule set.
func (r *Router) Schedules() []Schedule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Schedule(nil), r.schedules...)
}

// runSchedules fires every enabled schedule whose At/Days match now. Each
// schedule fires at most once per matching minute (guarded by schedLastFired),
// so a sub-minute ticker or clock jitter cannot double-fire it.
func (r *Router) runSchedules(ctx context.Context, now time.Time) {
	r.mu.RLock()
	scheds := append([]Schedule(nil), r.schedules...)
	r.mu.RUnlock()

	now = r.localNow(now)

	hhmm := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	stamp := now.Format("2006-01-02T15:04")
	weekday := int(now.Weekday())

	for _, s := range scheds {
		if !s.Enabled || s.At != hhmm || !ValidScheduleAction(s.Action) {
			continue
		}
		if len(s.Days) > 0 && !containsInt(s.Days, weekday) {
			continue
		}
		r.schedMu.Lock()
		if r.schedLastFired[s.ID] == stamp {
			r.schedMu.Unlock()
			continue // already fired this minute
		}
		if r.schedLastFired == nil {
			r.schedLastFired = make(map[string]string)
		}
		r.schedLastFired[s.ID] = stamp
		r.schedMu.Unlock()
		r.fireSchedule(ctx, s)
	}
}

func (r *Router) fireSchedule(ctx context.Context, s Schedule) {
	// found tracks whether the target node actually exists so a schedule
	// pointed at a stale/renamed/removed node doesn't log a misleading
	// "fired" line every tick while silently doing nothing. warmup/unload
	// dispatch to goroutines and log their own "node not found" diagnostic
	// (see WarmModels/UnloadModels), so only drain/undrain  --  whose result is
	// available synchronously  --  gate the summary log here.
	found := true
	switch s.Action {
	case "warmup":
		r.WarmModels(ctx, s.Node, s.Models)
	case "unload":
		r.UnloadModels(ctx, s.Node, s.Models)
	case "drain":
		found = r.DrainNode(s.Node, "scheduled")
	case "undrain":
		found = r.UndrainNode(s.Node)
	}
	metrics.ScheduleFired(s.Action, s.Node)
	if !found {
		log.Printf("schedule %q fired but node %q was not found: action=%s did nothing", s.ID, s.Node, s.Action)
		return
	}
	log.Printf("schedule %q fired: action=%s node=%s models=%v", s.ID, s.Action, s.Node, s.Models)
}

// WarmModels preloads the given models on a single node immediately via a real
// /api/generate keep_alive (used by scheduled warmup). Non-Ollama nodes and
// unknown node names are skipped.
func (r *Router) WarmModels(ctx context.Context, nodeName string, models []string) {
	r.mu.RLock()
	cfg := r.warmupCfg
	var target *NodeState
	for _, n := range r.nodes {
		if n.Name == nodeName {
			target = n
			break
		}
	}
	r.mu.RUnlock()
	if target == nil {
		log.Printf("scheduled warmup skipped: node %q not found", nodeName)
		return
	}
	if rt := target.GetRuntime(); rt != "ollama" && rt != "" {
		log.Printf("scheduled warmup skipped: node %q runtime %q does not support keep_alive warmup", nodeName, rt)
		return
	}
	keepAlive := effectiveKeepAlive(cfg.KeepAlive, time.Duration(cfg.IntervalMs)*time.Millisecond)
	for _, m := range models {
		if m == "" {
			continue
		}
		m := m
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[router] panic in goroutine: %v", r)
				}
			}()
			r.ensureHeadroom(ctx, target, m)
			status := "ok"
			if err := r.pingNode(ctx, target, m, keepAlive); err != nil {
				status = "error"
			}
			metrics.WarmupPing(m, target.Name, status)
		}()
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
