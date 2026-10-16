package router

import (
	"fmt"
	"sort"
	"time"
)

// SoleReplicaModel is a model that no other node could serve if the target
// node went away: the target is its only possible server.
type SoleReplicaModel struct {
	Model   string `json:"model"`
	Runtime string `json:"runtime"`
}

// ColdLoadModel is a model that is not warm on any other node, but that at
// least one Ollama peer would have to load on demand. It is a soft note and
// never blocks anything.
type ColdLoadModel struct {
	Model   string   `json:"model"`
	Runtime string   `json:"runtime"`
	Peers   []string `json:"peers"`
}

// SoleReplicaUnknown is a place where the answer cannot be verified from the
// data marbor holds. Model is empty for a node-level gap (the target itself
// cannot be read).
type SoleReplicaUnknown struct {
	Node   string `json:"node"`
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason"`
}

// SoleReplicaImpact is what taking one node out of service does to model
// availability on the rest of the fleet. Only Models is a hard finding.
type SoleReplicaImpact struct {
	Models       []SoleReplicaModel   `json:"models"`
	ColdLoadOnly []ColdLoadModel      `json:"cold_load_only"`
	Unknown      []SoleReplicaUnknown `json:"unknown"`
}

type peerVerdict int

const (
	verdictIneligible peerVerdict = iota
	verdictUnknown
	verdictWarm
	verdictCold
)

const runtimeOllama = "ollama"

// normalizeRuntime maps the empty runtime to Ollama, as routing does, and
// leaves "auto" (detection pending) as is.
func normalizeRuntime(rt string) string {
	if rt == "" {
		return runtimeOllama
	}
	return rt
}

// PollInterval is how often the router polls each node.
func (r *Router) PollInterval() time.Duration {
	return r.interval
}

// pollIsStale reports whether a poll timestamp is older than the window after
// which routing itself stops trusting a node's snapshot.
func (r *Router) pollIsStale(last time.Time) bool {
	if r.interval <= 0 {
		return false
	}
	return time.Since(last) > time.Duration(r.healthFailureThreshold)*r.interval
}

// safetyView is a one-lock snapshot of the fields the check reads.
type safetyView struct {
	node     *NodeState
	name     string
	runtime  string
	healthy  bool
	draining bool
	lastPoll time.Time
}

func snapshotSafetyNode(n *NodeState) safetyView {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return safetyView{
		node:     n,
		name:     n.Name,
		runtime:  normalizeRuntime(n.Runtime),
		healthy:  n.Healthy,
		draining: n.Draining,
		lastPoll: n.LastPollAt,
	}
}

// SoleReplicaImpact reports which models would lose every possible server if
// the named node were drained or its runtime restarted. alsoDown names other
// nodes already going away (for example mid-restart) that must not count as
// available. The second result is false when the node does not exist.
//
// It is read-only: it reads node snapshots one lock at a time, holds no
// router lock, and fires no webhooks. Each peer is snapshotted once and the
// classification helpers then re-lock individual nodes, so a poll landing
// mid-check can make the answer slightly stale; callers that need a change
// to follow the check atomically serialize it themselves.
//
// It mirrors the routing eligibility rules with three deliberate
// differences: a peer's remaining request capacity is ignored (a busy peer
// still serves), data marbor cannot verify is reported as unknown rather
// than guessed, and an unwarm Ollama peer only counts as able to cold-load a
// model when the target is an Ollama node too.
func (r *Router) SoleReplicaImpact(target string, alsoDown map[string]bool) (SoleReplicaImpact, bool) {
	nodes := r.Nodes()
	var tn *NodeState
	for _, n := range nodes {
		if n.Name == target {
			tn = n
			break
		}
	}
	if tn == nil {
		return SoleReplicaImpact{}, false
	}
	roles, heads := resolveSchedulingRolesAndHeads(nodes)
	down := unreachableReplicaHeads(nodes, roles, heads)
	tv := snapshotSafetyNode(tn)

	var impact SoleReplicaImpact
	if skip, gap := r.targetGate(tv, roles[target]); skip {
		if gap != nil {
			impact.Unknown = append(impact.Unknown, *gap)
		}
		return impact, true
	}

	var peers []safetyView
	for _, n := range nodes {
		if n == tn || alsoDown[n.Name] {
			continue
		}
		peers = append(peers, snapshotSafetyNode(n))
	}
	for _, model := range loadedModelNames(tn) {
		r.classifyModel(&impact, model, tv, peers, roles, down)
	}
	sortImpact(&impact)
	return impact, true
}

// targetGate decides whether the target needs no check at all (skip) and, for
// a target whose holdings cannot be read, the node-level gap to report.
func (r *Router) targetGate(tv safetyView, role SchedulingRole) (skip bool, gap *SoleReplicaUnknown) {
	if tv.draining || role == RoleWorker || role == RoleUnresolved {
		return true, nil
	}
	if !tv.healthy {
		return true, &SoleReplicaUnknown{Node: tv.name, Reason: fmt.Sprintf("node %q is not healthy, so which models it holds cannot be confirmed", tv.name)}
	}
	if tv.lastPoll.IsZero() || r.pollIsStale(tv.lastPoll) {
		return true, &SoleReplicaUnknown{Node: tv.name, Reason: fmt.Sprintf("node %q has not reported recently, so which models it holds cannot be confirmed", tv.name)}
	}
	return false, nil
}

func loadedModelNames(n *NodeState) []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	seen := make(map[string]bool, len(n.LoadedModels))
	var names []string
	for _, m := range n.LoadedModels {
		if m.Name == "" || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		names = append(names, m.Name)
	}
	return names
}

// classifyModel files one model held by the target into exactly one bucket:
// covered (nothing recorded), cold-load only, unknown, or sole.
func (r *Router) classifyModel(impact *SoleReplicaImpact, model string, tv safetyView, peers []safetyView, roles map[string]SchedulingRole, down map[string]*NodeState) {
	var coldPeers []string
	var unknowns []SoleReplicaUnknown
	for _, p := range peers {
		verdict, reason := r.judgePeer(p, model, tv.runtime, roles[p.name], down[p.name] != nil)
		switch verdict {
		case verdictWarm:
			return
		case verdictCold:
			coldPeers = append(coldPeers, p.name)
		case verdictUnknown:
			unknowns = append(unknowns, SoleReplicaUnknown{Node: p.name, Model: model, Reason: reason})
		}
	}
	switch {
	case len(coldPeers) > 0:
		sort.Strings(coldPeers)
		impact.ColdLoadOnly = append(impact.ColdLoadOnly, ColdLoadModel{Model: model, Runtime: tv.runtime, Peers: coldPeers})
	case len(unknowns) > 0:
		impact.Unknown = append(impact.Unknown, unknowns...)
	default:
		impact.Models = append(impact.Models, SoleReplicaModel{Model: model, Runtime: tv.runtime})
	}
}

// judgePeer decides what one other node means for one model of the target.
// Order matters: a node that cannot serve at all is dropped first, then data
// that cannot be trusted is reported unknown, then warm, then cold-loadable.
func (r *Router) judgePeer(p safetyView, model, targetRuntime string, role SchedulingRole, headUnreachable bool) (peerVerdict, string) {
	if !p.healthy || p.draining || role == RoleWorker || !r.isGPUGroupSufficient(p.node) {
		return verdictIneligible, ""
	}
	if reason := r.peerDataGap(p, targetRuntime, role, headUnreachable); reason != "" {
		return verdictUnknown, reason
	}
	if r.isModelWarm(p.node, model) {
		if targetRuntime == runtimeOllama && p.runtime != runtimeOllama {
			return verdictUnknown, "model is loaded there but only reachable through the OpenAI-compatible API"
		}
		return verdictWarm, ""
	}
	if p.runtime == runtimeOllama && targetRuntime == runtimeOllama {
		return verdictCold, ""
	}
	return verdictIneligible, ""
}

// peerDataGap returns why a peer's state cannot be trusted, or "" when it can.
func (r *Router) peerDataGap(p safetyView, targetRuntime string, role SchedulingRole, headUnreachable bool) string {
	switch {
	case p.lastPoll.IsZero():
		return "node has not been polled yet"
	case r.pollIsStale(p.lastPoll):
		return "node has not reported recently"
	case p.runtime == "auto" || targetRuntime == "auto":
		return "runtime is still being detected"
	case role == RoleUnresolved:
		return "replica declaration does not resolve"
	case role == RoleHead && headUnreachable:
		return "a replica member's agent is not answering"
	}
	return ""
}

func sortImpact(impact *SoleReplicaImpact) {
	sort.Slice(impact.Models, func(i, j int) bool { return impact.Models[i].Model < impact.Models[j].Model })
	sort.Slice(impact.ColdLoadOnly, func(i, j int) bool { return impact.ColdLoadOnly[i].Model < impact.ColdLoadOnly[j].Model })
	sort.Slice(impact.Unknown, func(i, j int) bool {
		a, b := impact.Unknown[i], impact.Unknown[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Model < b.Model
	})
}
