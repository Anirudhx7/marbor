package router

// explain.go - P41 per-request routing explainability.
//
// RoutingDecision/ScoreComponent surface, per request, the reason a node was
// selected and (for score-based picks) the exact weighted breakdown that
// produced the winning score. This is observability only: nothing here
// changes which node a request is routed to. scoreComponents is the single
// source of truth for the arithmetic - computeNodeScore sums its Value
// fields rather than recomputing the score independently, so the exposed
// breakdown is guaranteed to sum to the real score used for selection,
// including both penalties applied at their actual clamped value (not a
// nominal -50 if the floor at zero already absorbed part of it).

// ScoreComponent is one term contributing to a node's placement score.
type ScoreComponent struct {
	Name   string  `json:"name"`
	Raw    float64 `json:"raw"`
	Weight float64 `json:"weight"`
	// Value is the actual amount this term contributed to the final score.
	// For the two penalties this is the clamped delta actually applied
	// (e.g. -37 rather than -50 if the floor-at-zero guard cut it short),
	// not simply raw*weight.
	Value float64 `json:"value"`
}

// RoutingDecision is the winner-only explanation of one routing pick.
type RoutingDecision struct {
	Node   string `json:"node"`
	Reason string `json:"reason"` // session_affinity | pinned_warm | score_based
	Detail string `json:"detail,omitempty"`
	// AffinityLost is true when the request had a session-affinity entry
	// that was cleared (target node unhealthy/draining/ineligible) before
	// falling through to normal selection - so Reason is score_based or
	// pinned_warm but the request did not start out affinity-free.
	AffinityLost bool             `json:"affinityLost,omitempty"`
	Score        float64          `json:"score,omitempty"`
	Components   []ScoreComponent `json:"components,omitempty"` // score_based only
}

const (
	ReasonSessionAffinity = "session_affinity"
	ReasonPinnedWarm      = "pinned_warm"
	ReasonScoreBased      = "score_based"
)

func sumComponents(components []ScoreComponent) float64 {
	total := 0.0
	for _, c := range components {
		total += c.Value
	}
	return total
}
