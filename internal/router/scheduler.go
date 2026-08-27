package router

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
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

	// Last-run tracking, live runtime state only (never restored from
	// SQLite as authoritative - a fresh boot shows "never" until the next
	// fire, consistent with the State Hierarchy: live beats persisted).
	LastRunAt  string `json:"last_run_at,omitempty"` // RFC3339, UTC
	LastStatus string `json:"last_status,omitempty"` // "ok" | "error"
	LastError  string `json:"last_error,omitempty"`
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

	// Prune schedLastFired to only IDs still present in the new set -
	// otherwise every fired schedule ID accumulates in the map for the
	// process lifetime, since neither this replace nor RemoveNode ever
	// touches it.
	live := make(map[string]struct{}, len(s))
	for _, sched := range s {
		live[sched.ID] = struct{}{}
	}
	r.schedMu.Lock()
	for id := range r.schedLastFired {
		if _, ok := live[id]; !ok {
			delete(r.schedLastFired, id)
		}
	}
	r.schedMu.Unlock()
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
	// "fired" line every tick while silently doing nothing. asyncErr carries
	// the aggregate per-model outcome for warmup/unload, which WarmModels/
	// UnloadModels now wait for before returning - recordScheduleRun must
	// reflect what actually happened to each model, not just that dispatch
	// started, or an operator sees "last_status: ok" on a schedule whose
	// models all failed to warm/unload.
	found := true
	var asyncErr error
	switch s.Action {
	case "warmup":
		asyncErr = r.WarmModels(ctx, s.Node, s.Models)
	case "unload":
		asyncErr = r.UnloadModels(ctx, s.Node, s.Models)
	case "drain":
		found = r.DrainNode(s.Node, "scheduled")
	case "undrain":
		found = r.UndrainNode(s.Node)
	}
	metrics.ScheduleFired(s.Action, s.Node)
	if !found {
		errMsg := fmt.Sprintf("node %q not found", s.Node)
		log.Printf("schedule %q fired but node %q was not found: action=%s did nothing", s.ID, s.Node, s.Action)
		r.recordScheduleRun(s.ID, "error", errMsg)
		return
	}
	if asyncErr != nil {
		log.Printf("schedule %q fired: action=%s node=%s models=%v - %v", s.ID, s.Action, s.Node, s.Models, asyncErr)
		r.recordScheduleRun(s.ID, "error", asyncErr.Error())
		return
	}
	log.Printf("schedule %q fired: action=%s node=%s models=%v", s.ID, s.Action, s.Node, s.Models)
	r.recordScheduleRun(s.ID, "ok", "")
}

// recordScheduleRun stamps the outcome of a schedule dispatch onto the
// in-memory schedule so GET /admin/schedules can show "last ran" without a
// separate log/history store. For warmup/unload, status reflects the actual
// per-model outcome (WarmModels/UnloadModels wait for every model's dispatch
// before returning) - not just that the node was found, so a schedule whose
// models fail to warm/unload shows "error" here, not a false "ok".
func (r *Router) recordScheduleRun(id, status, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.schedules {
		if r.schedules[i].ID == id {
			r.schedules[i].LastRunAt = time.Now().UTC().Format(time.RFC3339)
			r.schedules[i].LastStatus = status
			r.schedules[i].LastError = errMsg
			break
		}
	}
}

// WarmModels preloads the given models on a single node immediately via a real
// /api/generate keep_alive (used by scheduled warmup). Non-Ollama nodes and
// unknown node names are skipped. Waits for every model's ping to finish and
// returns a non-nil error listing any that failed (also recorded into
// NodeState.WarmupErrors, same as the periodic keep-warm pinger in warmer.go)
// so the caller's schedule status reflects the real outcome, not just that
// dispatch started.
func (r *Router) WarmModels(ctx context.Context, nodeName string, models []string) error {
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
		return fmt.Errorf("node %q not found", nodeName)
	}
	if rt := target.GetRuntime(); rt != "ollama" && rt != "" {
		log.Printf("scheduled warmup skipped: node %q runtime %q does not support keep_alive warmup", nodeName, rt)
		return fmt.Errorf("node %q runtime %q does not support keep_alive warmup", nodeName, rt)
	}
	target.mu.RLock()
	draining := target.Draining
	target.mu.RUnlock()
	if draining {
		log.Printf("scheduled warmup skipped: node %q is draining", nodeName)
		return fmt.Errorf("node %q is draining", nodeName)
	}
	keepAlive := effectiveKeepAlive(cfg.KeepAlive, time.Duration(cfg.IntervalMs)*time.Millisecond)
	// A scheduled warmup is an explicit "be warm again" request - it must
	// override any suppression a prior manual/scheduled unload left behind,
	// else the model would stay cold forever despite this schedule firing.
	r.clearWarmupSuppress(target.Name, models...)
	// Models on the same node are warmed one at a time, not fired
	// concurrently - see pingWarmupModels (warmer.go) for why: concurrent
	// cold /api/generate loads against one node race ensureHeadroom's
	// capacity check (each sees the identical pre-warmup snapshot) and hand
	// the real runtime multiple competing loads it must arbitrate itself.
	var failures []string
	for _, m := range models {
		if m == "" {
			continue
		}
		r.ensureHeadroom(ctx, target, m)
		status := "ok"
		err := r.pingNode(ctx, target, m, keepAlive)
		if err != nil {
			status = "error"
			// Warmup failed - release the reservation now instead of
			// letting it block other models' headroom checks for the
			// remainder of warmReservationTTL (mirrors warmer.go's
			// pingWarmupModels).
			r.clearWarmReservation(target.Name, m)
			failures = append(failures, fmt.Sprintf("%s: %v", m, err))
		}
		target.Lock()
		if err != nil {
			if target.WarmupErrors == nil {
				target.WarmupErrors = map[string]string{}
			}
			target.WarmupErrors[m] = err.Error()
		} else {
			delete(target.WarmupErrors, m)
		}
		target.Unlock()
		metrics.WarmupPing(m, target.Name, status)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d model(s) failed to warm: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
