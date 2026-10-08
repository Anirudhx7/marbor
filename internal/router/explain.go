package router

import "slices"

// explain.go - per-request routing explainability.
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
	// Phase groups this term into one of three named optimization phases
	// (the Phase* constants below) - a structural label only, added on top
	// of the existing weighted-sum arithmetic. It does not change Raw,
	// Weight, Value, or which node wins; it exists purely so the terms
	// inside one flat sum can be read as "locality" vs "predicted
	// performance" vs "reliability" instead of an unordered list.
	Phase string `json:"phase,omitempty"`
}

const (
	PhaseLocality             = "locality"
	PhasePredictedPerformance = "predicted_performance"
	PhaseReliability          = "reliability"
)

// ExcludedCandidate is one node the pre-score hard filter (health, draining,
// runtime filter, model eligibility, per-node capacity, GPU-group shape)
// removed from candidacy before scoring ever ran, and why. Reason is a
// stable, machine-readable identifier (never a sentence) - CLI and UI each
// translate it to a natural-language display string; nothing downstream
// should string-match on prose here.
type ExcludedCandidate struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

const (
	ExcludeReasonUnhealthy            = "unhealthy"
	ExcludeReasonDraining             = "draining"
	ExcludeReasonRuntimeMismatch      = "runtime_mismatch"
	ExcludeReasonIneligibleModel      = "ineligible_model"
	ExcludeReasonOverCapacity         = "over_capacity"
	ExcludeReasonInsufficientGPUGroup = "insufficient_gpu_group"
	// ExcludeReasonReplicaWorker: this node is a confirmed non-head member
	// of a declared multi-host replica - route to its replica's head
	// instead. ExcludeReasonReplicaUnresolved: this node's replica_peers
	// declaration conflicts (or is one-sided) with another node's - needs
	// operator reconciliation before it can resolve to a role. Two distinct
	// reasons because the operator-facing remediation differs.
	ExcludeReasonReplicaWorker     = "replica_worker"
	ExcludeReasonReplicaUnresolved = "replica_unresolved"
	// ExcludeReasonReplicaMemberUnreachable: this node is the head of a
	// confirmed replica and the marbor agent of a non-head member's host has
	// stopped answering. It says the agent is dark, not that the worker
	// process is dead - the worker may still be serving. When excluding it
	// would leave no candidate at all the head is used anyway as a last
	// resort and the decision Detail says so.
	ExcludeReasonReplicaMemberUnreachable = "replica_member_unreachable"
)

// excludeReasons backs ExcludeReasons; unexported so callers cannot mutate it.
var excludeReasons = []string{
	ExcludeReasonUnhealthy,
	ExcludeReasonDraining,
	ExcludeReasonRuntimeMismatch,
	ExcludeReasonIneligibleModel,
	ExcludeReasonOverCapacity,
	ExcludeReasonInsufficientGPUGroup,
	ExcludeReasonReplicaWorker,
	ExcludeReasonReplicaUnresolved,
	ExcludeReasonReplicaMemberUnreachable,
}

// ExcludeReasons returns a copy of the list of every ExcludeReason constant,
// so a client that maps reasons to display text can assert it covers all of
// them.
func ExcludeReasons() []string {
	return slices.Clone(excludeReasons)
}

// maxExcludedCandidates bounds how many ExcludedCandidate entries a single
// RoutingDecision carries, so a large fleet routing a rarely-requested model
// can't put every node in the response. ExcludedTotal (below) reports the
// real count when this cap truncates the list. The cap does not apply to
// replica heads kept as a last resort (replica_member_unreachable): they are
// always listed, even past the cap, so the head a request was routed to is
// never missing from its explanation. Such a list can therefore exceed the
// cap by the number of those heads.
const maxExcludedCandidates = 20

// RoutingDecision is the winner-only explanation of one routing pick, plus
// (additively) which candidates were eliminated before scoring and why.
type RoutingDecision struct {
	Node   string `json:"node"`
	Reason string `json:"reason"` // session_affinity | pinned_warm | score_based
	Detail string `json:"detail,omitempty"`
	// AffinityLost is true when the request had a session-affinity entry
	// that was cleared (target node unhealthy/draining/ineligible) or, for a
	// replica head whose member host agent is not answering, bypassed with
	// the pin kept, before falling through to normal selection - so Reason is
	// score_based or pinned_warm but the request did not start out
	// affinity-free.
	AffinityLost bool             `json:"affinityLost,omitempty"`
	Score        float64          `json:"score,omitempty"`
	Components   []ScoreComponent `json:"components,omitempty"` // score_based only
	// Excluded lists up to maxExcludedCandidates real candidates the
	// pre-score hard filter removed before scoring, in the order they were
	// evaluated; a replica head that can be a last resort is always listed,
	// even past that cap. Never populated for the session_affinity fast path (it
	// never runs the candidate loop). When the last-resort path routed to a
	// replica head whose member host agent is not answering, the chosen node
	// itself also appears here with replica_member_unreachable, and Detail
	// says it was a last resort.
	Excluded []ExcludedCandidate `json:"excluded,omitempty"`
	// ExcludedTotal is only set (non-zero) when the real number of excluded
	// candidates exceeds the number listed - omitted entirely in the
	// common case where every excluded candidate is already listed.
	ExcludedTotal int `json:"excludedTotal,omitempty"`
	// lastResort is set when the node was chosen only because no other
	// candidate remained and its replica member's host agent is dark.
	lastResort bool
}

const (
	ReasonSessionAffinity = "session_affinity"
	ReasonPinnedWarm      = "pinned_warm"
	ReasonScoreBased      = "score_based"
	// ReasonNoCandidate means every node was removed by the pre-score hard
	// filter, so no scoring ever ran. Node is empty; Excluded/ExcludedTotal
	// carry the only useful information about why the request had nowhere
	// to go.
	ReasonNoCandidate = "no_candidate"
)

func sumComponents(components []ScoreComponent) float64 {
	total := 0.0
	for _, c := range components {
		total += c.Value
	}
	return total
}
