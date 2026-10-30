package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

	// acknowledgeSoleParam is the optional repeated query parameter that names
	// the models the caller was shown as having no other server. Sent together
	// with force=true it makes the force conditional on that list still being
	// the whole current list.
	acknowledgeSoleParam = "acknowledge_sole"

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

// soleConsent is what the caller said about the sole-replica check: whether it
// sent force=true, whether it sent the acknowledge_sole key at all (even with an
// empty value, which is an empty acknowledged list), and the model names it
// acknowledged. parseErr is set when the query string did not parse; the check
// then fails closed if any model has no other server.
type soleConsent struct {
	force    bool
	ackGiven bool
	acked    map[string]bool
	parseErr error
}

// soleConsentFrom reads the consent from the request's raw query. Only the exact
// value force=true counts as force; 1, yes and TRUE are treated as absent. Ack
// names are compared as exact strings.
func soleConsentFrom(r *http.Request) soleConsent {
	values, err := url.ParseQuery(r.URL.RawQuery)
	c := soleConsent{parseErr: err, acked: map[string]bool{}}
	if err != nil {
		// ParseQuery keeps the pairs that did parse, but a request with a
		// broken query is never trusted as consent.
		return c
	}
	c.force = values.Get(forceParam) == "true"
	if acks, present := values[acknowledgeSoleParam]; present {
		c.ackGiven = true
		for _, a := range acks {
			if a != "" {
				c.acked[a] = true
			}
		}
	}
	return c
}

// unacknowledged returns the sole models the caller did not acknowledge.
func (c soleConsent) unacknowledged(models []router.SoleReplicaModel) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range models {
		if !c.acked[m.Model] && !seen[m.Model] {
			seen[m.Model] = true
			out = append(out, m.Model)
		}
	}
	return out
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
	soleStaleAck                       // force with an acknowledged list that misses a current sole model; nothing was changed
	soleBadQuery                       // sole models and a query string that did not parse; nothing was changed
)

// soleCheck is the result of checkSoleReplica.
type soleCheck struct {
	impact         router.SoleReplicaImpact
	outcome        soleOutcome
	unacknowledged []string // soleStaleAck: current sole models the caller did not acknowledge
	ackEmpty       bool     // soleStaleAck: the key was sent but named no model
	reviewed       bool     // soleForced: the acknowledge_sole key was sent
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
func (s *Server) checkSoleReplica(name string, consent soleConsent, commit func()) soleCheck {
	st := &s.soleReplica
	st.mu.Lock()
	defer st.mu.Unlock()
	impact, ok := s.router.SoleReplicaImpact(name, s.restartingNodesLocked(time.Now()))
	if !ok {
		return soleCheck{outcome: soleNodeMissing}
	}
	if len(impact.Models) == 0 {
		commit()
		if st.afterCommit != nil {
			st.afterCommit()
		}
		return soleCheck{impact: impact, outcome: soleProceed}
	}
	switch {
	case consent.parseErr != nil:
		return soleCheck{impact: impact, outcome: soleBadQuery}
	case !consent.force:
		return soleCheck{impact: impact, outcome: soleBlocked}
	}
	// force=true with the acknowledge_sole key: proceed only if every current
	// sole model was acknowledged. Without the key force overrides unchecked.
	// The comparison and the commit share this lock, so the list that was
	// acknowledged is the list the change is made against.
	if consent.ackGiven {
		if missing := consent.unacknowledged(impact.Models); len(missing) > 0 {
			return soleCheck{impact: impact, outcome: soleStaleAck, unacknowledged: missing, ackEmpty: len(consent.acked) == 0}
		}
	}
	commit()
	if st.afterCommit != nil {
		st.afterCommit()
	}
	return soleCheck{impact: impact, outcome: soleForced, reviewed: consent.ackGiven}
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
	sole := s.checkSoleReplica(nodeName, soleConsentFrom(r), func() { s.markRestartStarted(nodeName) })
	if !sole.found() {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return sole, noop, false
	}
	if writeSoleRefusal(w, nodeName, action, sole) {
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
	// Purely defensive: decrement only while a restart is counted in flight, so
	// the counter cannot go negative if the marker was recreated or dropped (by
	// the TTL) while an older restart was still running. A poll cannot confirm
	// the marker away while inflight is above zero, because the marker only gets
	// a returned time once the last in-flight restart has ended.
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

// agentRefusedError is an error that means the agent turned the action down
// before running it: it answered with a 4xx status other than 408 and 499
// (those two are what a proxy may emit after the request was passed on). Any 5xx
// is not this type, not even one with a readable error body: the agent answers
// 502 when a restart failed after it had already stopped the runtime. Transport
// failures, timeouts, redirects and unreadable replies are not this type either:
// the action may have run.
type agentRefusedError struct{ msg string }

func (e *agentRefusedError) Error() string { return e.msg }

// restartOutcomeUnclearError wraps an agent failure of a restart that may still
// have taken the runtime down, so the caller sees that the outcome is unclear
// rather than a plain refusal. Start and stop messages are never wrapped.
func restartOutcomeUnclearError(err error) error {
	return fmt.Errorf("%w; restart outcome unclear: the runtime may be restarting, check the node before retrying (the restart record clears on its own after the next poll, or after %s)", err, restartMarkerTTL)
}

// soleReplicaMessage is the human text of the 409.
func soleReplicaMessage(name string, action soleAction, models []router.SoleReplicaModel) string {
	subject := fmt.Sprintf("draining node %q", name)
	if action == soleActionRestart {
		subject = fmt.Sprintf("restarting the runtime on node %q", name)
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Model)
	}
	return fmt.Sprintf("%s would leave %d model(s) with no other node that can serve them: %s. Pass force=true (CLI: --force) to proceed; only the exact value force=true is accepted.", subject, len(models), quotedList(names))
}

// quotedList quotes up to maxModelsInMessage names and counts the rest.
func quotedList(names []string) string {
	shown := make([]string, 0, maxModelsInMessage)
	for i, n := range names {
		if i == maxModelsInMessage {
			break
		}
		shown = append(shown, fmt.Sprintf("%q", n))
	}
	list := strings.Join(shown, ", ")
	if extra := len(names) - len(shown); extra > 0 {
		list += fmt.Sprintf(" and %d more", extra)
	}
	return list
}

// staleAckMessage is the 409 text when the acknowledged list no longer covers
// every model with no other server.
func staleAckMessage(name string, action soleAction, unacknowledged []string, ackEmpty bool) string {
	subject := fmt.Sprintf("draining node %q", name)
	if action == soleActionRestart {
		subject = fmt.Sprintf("restarting the runtime on node %q", name)
	}
	msg := fmt.Sprintf("%s was not done: the models with no other server changed since you reviewed them: %s; review and retry.", subject, quotedList(unacknowledged))
	if ackEmpty {
		msg += " acknowledge_sole was sent with no model name, which counts as an empty acknowledged list."
	}
	return msg
}

// refused reports that nothing was changed because of the check: a plain block,
// a stale acknowledgement or an unparseable query.
func (c soleCheck) refused() bool {
	return c.outcome == soleBlocked || c.outcome == soleStaleAck || c.outcome == soleBadQuery
}

// writeSoleRefusal writes the response for a check that changed nothing: 409
// for a block or a stale acknowledgement, 400 for an unparseable query. It
// reports whether it wrote one, so each caller must stop when it returns true.
func writeSoleRefusal(w http.ResponseWriter, name string, action soleAction, sole soleCheck) bool {
	switch sole.outcome {
	case soleBlocked:
		writeSoleReplicaConflict(w, name, action, sole.impact)
	case soleStaleAck:
		writeStaleAckConflict(w, name, action, sole)
	case soleBadQuery:
		writeJSONError(w, http.StatusBadRequest, "malformed query string: the sole-replica check cannot read force or acknowledge_sole, so nothing was changed")
	default:
		return false
	}
	return true
}

// writeStaleAckConflict writes the 409 for force with an acknowledged list that
// misses a current sole model. It carries the current lists and the models that
// were not acknowledged.
func writeStaleAckConflict(w http.ResponseWriter, name string, action soleAction, sole soleCheck) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":          staleAckMessage(name, action, sole.unacknowledged, sole.ackEmpty),
		"code":           soleReplicaCode,
		"sole_replica":   soleReplicaBody{soleReplicaLists: newSoleReplicaLists(sole.impact), Forced: false},
		"unacknowledged": sole.unacknowledged,
	})
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
// were taken out of service, whether force was needed to do it, and whether the
// force came from a reviewed confirm (the acknowledge_sole key was sent). The
// acknowledged names themselves are never written.
func soleReplicaAuditDetail(detail string, impact router.SoleReplicaImpact, forced, reviewed bool) string {
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
		if reviewed {
			out += ", Reviewed: true"
		}
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
