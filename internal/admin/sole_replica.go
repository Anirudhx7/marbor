package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/router"
)

const (
	// soleReplicaCode is the machine-readable "code" on the 409 returned when
	// a drain or runtime restart would leave a model with no node that can
	// serve it and the request did not pass force=true.
	soleReplicaCode = "sole_replica"

	// forceParam is the query parameter that proceeds past the sole-replica
	// check (CLI: --force). Only the exact value "true" counts.
	forceParam = "force"

	// restartMarkerTTL bounds how long a restart keeps its node counted as
	// unavailable when no later poll confirms it is back.
	restartMarkerTTL = 15 * time.Minute

	// maxModelsInMessage caps how many model names the 409 error text lists;
	// the full list is always in the sole_replica body.
	maxModelsInMessage = 5
)

// soleAction is an action the sole-replica check guards. It appears in the 409
// text.
type soleAction string

const (
	soleActionDrain   soleAction = "drain"
	soleActionRestart soleAction = "restart"
)

// restartMarker records a runtime restart marbor sent, so a drain or restart
// of a peer right afterwards does not count the restarting node as a spare.
// returned is zero while any restart of the node is in flight. maybeDown is set
// once a restart ended without the agent refusing it: the runtime may have
// restarted, so the marker must stay until a later poll or the TTL clears it.
type restartMarker struct {
	started   time.Time
	returned  time.Time
	inflight  int // restarts of this node whose agent call has not ended
	maybeDown bool
}

// soleReplicaState serializes the sole-replica check with the state change it
// guards, so two requests against mutual peers cannot both pass. The zero
// value is ready to use. It is best effort: it covers one marbor process and
// only changes made through this API, not scheduled or thermal drains and not
// restarts done outside marbor. Markers are lost on process restart.
type soleReplicaState struct {
	mu         sync.Mutex
	restarting map[string]restartMarker

	// afterCommit, when set, runs inside the lock right after a change is
	// committed. Tests use it to hold a request mid-commit.
	afterCommit func()
}

// soleReplicaLists is the wire form of a router.SoleReplicaImpact. Every list
// is always present (empty, never null).
type soleReplicaLists struct {
	Models       []router.SoleReplicaModel   `json:"models"`
	ColdLoadOnly []router.ColdLoadModel      `json:"cold_load_only"`
	Unknown      []router.SoleReplicaUnknown `json:"unknown"`
}

// soleReplicaBody is the sole_replica object on a drain or restart response.
// Forced is true only when force=true was sent and it mattered.
type soleReplicaBody struct {
	soleReplicaLists
	Forced bool `json:"forced"`
}

func newSoleReplicaLists(impact router.SoleReplicaImpact) soleReplicaLists {
	l := soleReplicaLists{Models: impact.Models, ColdLoadOnly: impact.ColdLoadOnly, Unknown: impact.Unknown}
	if l.Models == nil {
		l.Models = []router.SoleReplicaModel{}
	}
	if l.ColdLoadOnly == nil {
		l.ColdLoadOnly = []router.ColdLoadModel{}
	}
	if l.Unknown == nil {
		l.Unknown = []router.SoleReplicaUnknown{}
	}
	return l
}

func impactIsEmpty(impact router.SoleReplicaImpact) bool {
	return len(impact.Models)+len(impact.ColdLoadOnly)+len(impact.Unknown) == 0
}

// soleReplicaField is the value for the "sole_replica" key on a 200 response,
// or nil when there is nothing to report (the key is then omitted).
func soleReplicaField(impact router.SoleReplicaImpact, forced bool) *soleReplicaBody {
	if impactIsEmpty(impact) {
		return nil
	}
	return &soleReplicaBody{soleReplicaLists: newSoleReplicaLists(impact), Forced: forced}
}

// forceRequested reports whether the request carries force=true. Any other
// value, including 1, yes and TRUE, is treated as absent.
func forceRequested(r *http.Request) bool {
	return r.URL.Query().Get(forceParam) == "true"
}

// soleOutcome is how a sole-replica check ended. One value, so the impossible
// combinations (blocked and forced at once) cannot be represented.
type soleOutcome int

const (
	soleNotChecked  soleOutcome = iota // no check ran (the zero value)
	soleNodeMissing                    // the node does not exist
	soleProceed                        // nothing needed force; the change was made
	soleForced                         // force was sent and sole models existed; the change was made
	soleBlocked                        // sole models and no force; nothing was changed
)

// soleCheck is the result of checkSoleReplica.
type soleCheck struct {
	impact  router.SoleReplicaImpact
	outcome soleOutcome
}

func (c soleCheck) found() bool   { return c.outcome != soleNodeMissing }
func (c soleCheck) blocked() bool { return c.outcome == soleBlocked }
func (c soleCheck) forced() bool  { return c.outcome == soleForced }

// checkSoleReplica evaluates the impact of taking name out of service and,
// unless that is blocked, runs commit while still holding the lock, so two
// requests against mutual peers cannot both pass. The impact is computed from
// a snapshot of each peer, and the router helpers re-lock per node, so a poll
// landing mid-check can make the answer slightly stale; the lock serializes
// marbor's own changes, not the fleet. Callers must not hold the lock.
func (s *Server) checkSoleReplica(name string, force bool, commit func()) soleCheck {
	st := &s.soleReplica
	st.mu.Lock()
	defer st.mu.Unlock()
	impact, ok := s.router.SoleReplicaImpact(name, s.restartingNodesLocked(time.Now()))
	if !ok {
		return soleCheck{outcome: soleNodeMissing}
	}
	if len(impact.Models) > 0 && !force {
		return soleCheck{impact: impact, outcome: soleBlocked}
	}
	commit()
	if st.afterCommit != nil {
		st.afterCommit()
	}
	if force && len(impact.Models) > 0 {
		return soleCheck{impact: impact, outcome: soleForced}
	}
	return soleCheck{impact: impact, outcome: soleProceed}
}

// guardRuntimeActionSole runs the sole-replica check for a runtime action. Only
// a restart is checked: stop and start pass straight through with a no-op
// finish. When the restart may proceed it has already written the restart
// marker, and finish must be called once the agent call ends: finish(true) only
// when the agent definitely refused the restart (so the runtime was never taken
// down), finish(false) for success and for any outcome that leaves it unknown,
// such as a timeout or transport failure. When it may not proceed the response
// has been written.
func (s *Server) guardRuntimeActionSole(w http.ResponseWriter, r *http.Request, nodeName string, action soleAction) (soleCheck, func(refused bool), bool) {
	noop := func(bool) {}
	if action != soleActionRestart {
		return soleCheck{}, noop, true
	}
	sole := s.checkSoleReplica(nodeName, forceRequested(r), func() { s.markRestartStarted(nodeName) })
	switch {
	case !sole.found():
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return sole, noop, false
	case sole.blocked():
		writeSoleReplicaConflict(w, nodeName, action, sole.impact)
		return sole, noop, false
	}
	return sole, func(refused bool) { s.finishRestart(nodeName, refused) }, true
}

// restartingNodesLocked returns the nodes still counted as unavailable after a
// restart, dropping markers that have expired or been confirmed over. A marker
// is confirmed over once the node has been polled after the restart returned
// plus one poll interval (a poll already in flight when the restart returned
// can still carry the old models). Caller holds soleReplica.mu.
func (s *Server) restartingNodesLocked(now time.Time) map[string]bool {
	st := &s.soleReplica
	if len(st.restarting) == 0 {
		return nil
	}
	lastPoll := make(map[string]time.Time, len(st.restarting))
	for _, n := range s.router.Nodes() {
		if _, tracked := st.restarting[n.Name]; tracked {
			n.RLock()
			lastPoll[n.Name] = n.LastPollAt
			n.RUnlock()
		}
	}
	interval := s.router.PollInterval()
	out := make(map[string]bool, len(st.restarting))
	for name, mk := range st.restarting {
		expired := now.Sub(mk.started) > restartMarkerTTL
		confirmed := !mk.returned.IsZero() && lastPoll[name].After(mk.returned.Add(interval))
		if expired || confirmed {
			delete(st.restarting, name)
			continue
		}
		out[name] = true
	}
	return out
}

// markRestartStarted records that a restart of name is about to be sent. A
// restart already in flight for the node keeps its marker; this one joins it.
// Caller holds soleReplica.mu (it runs as the commit step of checkSoleReplica).
func (s *Server) markRestartStarted(name string) {
	st := &s.soleReplica
	if st.restarting == nil {
		st.restarting = make(map[string]restartMarker)
	}
	mk := st.restarting[name]
	mk.inflight++
	mk.started = time.Now()
	mk.returned = time.Time{}
	st.restarting[name] = mk
}

// finishRestart ends one restart of name. The marker is deleted only when no
// other restart of the node is still in flight and none ended without a
// definite agent refusal: a refused restart never took the runtime down, but a
// timeout, cancel or transport failure may have come after it did, so those
// keep the marker (refused is false) until a later poll confirms the node or
// the TTL passes.
func (s *Server) finishRestart(name string, refused bool) {
	st := &s.soleReplica
	st.mu.Lock()
	defer st.mu.Unlock()
	mk, ok := st.restarting[name]
	if !ok {
		return
	}
	if mk.inflight > 0 {
		mk.inflight--
	}
	if !refused {
		mk.maybeDown = true
	}
	if mk.inflight == 0 {
		if !mk.maybeDown {
			delete(st.restarting, name)
			return
		}
		mk.returned = time.Now()
	}
	st.restarting[name] = mk
}

// agentRefusedError is an error the agent itself reported for a runtime action:
// it answered with an error status or message, so the action did not start.
// Transport failures, timeouts and undecodable replies are not this type: the
// action may have run.
type agentRefusedError struct{ msg string }

func (e *agentRefusedError) Error() string { return e.msg }

// soleReplicaMessage is the human text of the 409.
func soleReplicaMessage(name string, action soleAction, models []router.SoleReplicaModel) string {
	subject := fmt.Sprintf("draining node %q", name)
	if action == soleActionRestart {
		subject = fmt.Sprintf("restarting the runtime on node %q", name)
	}
	shown := make([]string, 0, maxModelsInMessage)
	for i, m := range models {
		if i == maxModelsInMessage {
			break
		}
		shown = append(shown, fmt.Sprintf("%q", m.Model))
	}
	list := strings.Join(shown, ", ")
	if extra := len(models) - len(shown); extra > 0 {
		list += fmt.Sprintf(" and %d more", extra)
	}
	return fmt.Sprintf("%s would leave %d model(s) with no other node that can serve them: %s. Pass force=true (CLI: --force) to proceed; only the exact value force=true is accepted.", subject, len(models), list)
}

// writeSoleReplicaConflict writes the 409 for a blocked drain or restart.
func writeSoleReplicaConflict(w http.ResponseWriter, name string, action soleAction, impact router.SoleReplicaImpact) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":        soleReplicaMessage(name, action, impact.Models),
		"code":         soleReplicaCode,
		"sole_replica": soleReplicaBody{soleReplicaLists: newSoleReplicaLists(impact), Forced: false},
	})
}

// soleReplicaAuditDetail extends an audit detail with the sole models that
// were taken out of service, and whether force was needed to do it.
func soleReplicaAuditDetail(detail string, impact router.SoleReplicaImpact, forced bool) string {
	if len(impact.Models) == 0 {
		return detail
	}
	names := make([]string, 0, len(impact.Models))
	for _, m := range impact.Models {
		names = append(names, m.Model)
	}
	// %q so a model name containing commas or newlines cannot forge extra
	// key: value fields in the audit detail.
	out := fmt.Sprintf("%s, Sole replica models: %q", detail, names)
	if forced {
		out += ", Forced: true"
	}
	return out
}

// handleNodeSoleReplica is GET /admin/nodes/{name}/sole-replica: the same
// check a drain or restart runs, with no change to the node. Reading the
// restart markers also drops expired or confirmed ones, which is idempotent
// and happens under the same lock.
func (s *Server) handleNodeSoleReplica(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st := &s.soleReplica
	st.mu.Lock()
	impact, ok := s.router.SoleReplicaImpact(name, s.restartingNodesLocked(time.Now()))
	st.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"node":         name,
		"sole_replica": newSoleReplicaLists(impact),
	})
}
