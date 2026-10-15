package admin

import (
	"fmt"
	"net/http"
	"strings"
)

// vramPressureThreshold is the used/total VRAM ratio at or above which a node
// carries the "pressure" signal. It only drives this signal; the dashboard's
// VRAM bar colours itself independently.
const vramPressureThreshold = 0.90

// Node signal ids, in the canonical order they appear in a node's signals
// array and in error messages.
const (
	signalHealthy  = "healthy"
	signalDegraded = "degraded"
	signalDraining = "draining"
	signalDown     = "down"
	signalAgent    = "agent"
	signalWarm     = "warm"
	signalPressure = "pressure"
)

var nodeSignalIDs = []string{
	signalHealthy, signalDegraded, signalDraining, signalDown,
	signalAgent, signalWarm, signalPressure,
}

// NodeSignalIDs returns the valid signal ids in canonical order. The slice is
// a copy; callers may modify it.
func NodeSignalIDs() []string {
	return append([]string(nil), nodeSignalIDs...)
}

// nodeSignals classifies one built node response into the signals it matches,
// in canonical order. It is the single definition shared by the node list,
// the single-node endpoint, the ?signal= filter, the dashboard chips and the
// CLI, so none of them can drift apart.
//
//   - healthy and degraded exclude draining nodes: a draining node lights
//     "draining", not "healthy".
//   - down does not exclude draining: a node that is both matches both.
//   - agent means a live agent is reporting; a stale agent does not match.
//   - warm means at least one model is loaded.
//   - pressure needs a measured or declared total (never "none") and a
//     used/total ratio at or above vramPressureThreshold.
//
// The result is never nil so it always encodes as a JSON array.
func nodeSignals(n nodeResp) []string {
	out := make([]string, 0, len(nodeSignalIDs))
	if n.Health == "healthy" && !n.Draining {
		out = append(out, signalHealthy)
	}
	if n.Health == "degraded" && !n.Draining {
		out = append(out, signalDegraded)
	}
	if n.Draining {
		out = append(out, signalDraining)
	}
	if n.Health == "down" {
		out = append(out, signalDown)
	}
	if n.AgentPresent {
		out = append(out, signalAgent)
	}
	if len(n.LoadedModels) > 0 {
		out = append(out, signalWarm)
	}
	if n.VRAMSource != "none" && n.VRAMTotalMB > 0 &&
		float64(n.VRAMUsedMB)/float64(n.VRAMTotalMB) >= vramPressureThreshold {
		out = append(out, signalPressure)
	}
	return out
}

// parseSignalFilter reads every ?signal= value (repeated parameters and comma
// lists both work), trims, drops empty elements and lowercases. An empty
// result means "no filter". An unknown id returns a non-empty error message
// listing the valid ids.
func parseSignalFilter(values []string) (map[string]bool, string) {
	want := map[string]bool{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			id := strings.ToLower(strings.TrimSpace(part))
			if id == "" {
				continue
			}
			if !isNodeSignalID(id) {
				return nil, fmt.Sprintf("unknown signal %q: valid signals are %s", truncateSignalID(id), strings.Join(nodeSignalIDs, ", "))
			}
			want[id] = true
		}
	}
	return want, ""
}

// truncateSignalID caps an echoed, client-supplied id so an error message
// stays small.
func truncateSignalID(id string) string {
	const maxEcho = 64
	if len(id) <= maxEcho {
		return id
	}
	return id[:maxEcho] + "..."
}

func isNodeSignalID(id string) bool {
	for _, s := range nodeSignalIDs {
		if s == id {
			return true
		}
	}
	return false
}

// nodeFilter is the parsed ?signal= and ?q= query of the node list.
type nodeFilter struct {
	signals map[string]bool
	query   string // trimmed, lowercased
}

// parseNodeFilter parses the node list filter query. On a bad signal id it
// writes the 400 itself and returns ok=false.
func parseNodeFilter(w http.ResponseWriter, r *http.Request) (nodeFilter, bool) {
	q := r.URL.Query()
	signals, msg := parseSignalFilter(q["signal"])
	if msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return nodeFilter{}, false
	}
	return nodeFilter{signals: signals, query: strings.ToLower(strings.TrimSpace(q.Get("q")))}, true
}

func (f nodeFilter) active() bool {
	return len(f.signals) > 0 || f.query != ""
}

// matches reports whether a built node passes the filter: any one of the
// requested signals (OR), and the search text in the name or GPU model
// (case-insensitive substring, the same fields the dashboard search uses).
func (f nodeFilter) matches(n nodeResp) bool {
	if len(f.signals) > 0 {
		hit := false
		for _, s := range n.Signals {
			if f.signals[s] {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if f.query != "" {
		return strings.Contains(strings.ToLower(n.Name), f.query) ||
			strings.Contains(strings.ToLower(n.GPUModel), f.query)
	}
	return true
}

// apply returns the nodes that pass the filter as a new slice (never nil, so
// an empty result encodes as []). Node ids are untouched: they keep the
// position they have in the full fleet.
func (f nodeFilter) apply(nodes []nodeResp) []nodeResp {
	out := make([]nodeResp, 0, len(nodes))
	for _, n := range nodes {
		if f.matches(n) {
			out = append(out, n)
		}
	}
	return out
}
