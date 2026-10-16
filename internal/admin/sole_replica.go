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

// restartMarker records a runtime restart marbor sent, so a drain or restart
// of a peer right afterwards does not count the restarting node as a spare.
// returned is zero while the agent call is in flight.
type restartMarker struct {
	started  time.Time
	returned time.Time
}

// soleReplicaState serializes the sole-replica check with the state change it
// guards, so two requests against mutual peers cannot both pass. The zero
// value is ready to use. It is best effort: it covers one marbor process and
// only changes made through this API, not scheduled or thermal drains and not
// restarts done outside marbor. Markers are lost on process restart.
type soleReplicaState struct {
	mu         sync.Mutex
	restarting map[string]restartMarker
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

// soleCheck is the outcome of checkSoleReplica.
type soleCheck struct {
	impact  router.SoleReplicaImpact
	found   bool // the node exists
	blocked bool // sole models and no force: nothing was changed
	forced  bool // force was sent and sole models existed
}

// checkSoleReplica evaluates the impact of taking name out of service and,
// unless that is blocked, runs commit while still holding the lock. commit
// returns false when the node has vanished. Callers must not hold the lock.
func (s *Server) checkSoleReplica(name string, force bool, commit func() bool) soleCheck {
	st := &s.soleReplica
	st.mu.Lock()
	defer st.mu.Unlock()
	impact, ok := s.router.SoleReplicaImpact(name, s.restartingNodesLocked(time.Now()))
	if !ok {
		return soleCheck{}
	}
	if len(impact.Models) > 0 && !force {
		return soleCheck{impact: impact, found: true, blocked: true}
	}
	if !commit() {
		return soleCheck{}
	}
	return soleCheck{impact: impact, found: true, forced: force && len(impact.Models) > 0}
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

// markRestartStarted records that a restart of name is about to be sent.
// Caller holds soleReplica.mu (it runs as the commit step of checkSoleReplica).
func (s *Server) markRestartStarted(name string) bool {
	st := &s.soleReplica
	if st.restarting == nil {
		st.restarting = make(map[string]restartMarker)
	}
	st.restarting[name] = restartMarker{started: time.Now()}
	return true
}

// markRestartReturned notes that the agent call for name has finished. The
// marker stays until a later poll shows the node has come back.
func (s *Server) markRestartReturned(name string) {
	st := &s.soleReplica
	st.mu.Lock()
	defer st.mu.Unlock()
	if mk, ok := st.restarting[name]; ok {
		mk.returned = time.Now()
		st.restarting[name] = mk
	}
}

// soleReplicaMessage is the human text of the 409.
func soleReplicaMessage(name, action string, models []router.SoleReplicaModel) string {
	subject := fmt.Sprintf("draining node %q", name)
	if action == "restart" {
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
func writeSoleReplicaConflict(w http.ResponseWriter, name, action string, impact router.SoleReplicaImpact) {
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
// check a drain or restart runs, with no state change.
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
