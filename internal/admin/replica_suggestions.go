package admin

// replica_suggestions.go - the Admin API for replica suggestions: list what the
// agents' multi-host launch evidence suggests, confirm one (declare the same
// replica_peers on every member in one all-or-nothing step), and dismiss or
// restore a suggestion. Detection never writes anything by itself; only
// confirm does, and it writes ordinary declared replica_peers that the
// operator can clear again like any other declaration.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	// replicaSuggestionsDismissedKey holds the dismissals as a JSON string
	// array in the settings table, oldest first. An entry is "fingerprint:state"
	// (the suggestion's state when it was dismissed); an older plain
	// "fingerprint" entry hides the suggestion in any state.
	replicaSuggestionsDismissedKey = "replica_suggestions_dismissed"
	// maxDismissedSuggestions caps how many dismissed fingerprints are
	// remembered; the oldest is forgotten first.
	maxDismissedSuggestions = 100
	// maxSuggestionBodyBytes bounds the optional JSON body of confirm and dismiss.
	maxSuggestionBodyBytes = 64 << 10
)

// knownSuggestionStates are the states a dismissal may record.
var knownSuggestionStates = map[string]bool{
	router.SuggestionComplete:            true,
	router.SuggestionIncomplete:          true,
	router.SuggestionConflicting:         true,
	router.SuggestionContradictsDeclared: true,
}

type replicaSuggestionEvidenceResp struct {
	Node   string `json:"node"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

type replicaSuggestionDeclaredResp struct {
	Node    string   `json:"node"`
	Members []string `json:"members"`
	Head    string   `json:"head"`
}

type replicaSuggestionResp struct {
	Fingerprint string                          `json:"fingerprint"`
	Launcher    string                          `json:"launcher"`
	Runtime     string                          `json:"runtime"`
	State       string                          `json:"state"`
	Reason      string                          `json:"reason"`
	Head        string                          `json:"head"`
	Members     []string                        `json:"members"`
	Evidence    []replicaSuggestionEvidenceResp `json:"evidence"`
	Missing     []string                        `json:"missing"`
	Declared    []replicaSuggestionDeclaredResp `json:"declared"`
	Confirmable bool                            `json:"confirmable"`
	Dismissed   bool                            `json:"dismissed"`
}

type topologyCoverageResp struct {
	Node     string `json:"node"`
	Host     string `json:"host"`
	State    string `json:"state"`
	Detected bool   `json:"detected"`
	Detail   string `json:"detail"`
}

type replicaSuggestionsListResp struct {
	Suggestions    []replicaSuggestionResp `json:"suggestions"`
	Coverage       []topologyCoverageResp  `json:"coverage"`
	DismissedCount int                     `json:"dismissedCount"`
}

type replicaSuggestionRoleResp struct {
	Node string `json:"node"`
	Role string `json:"role"`
	Head string `json:"head"`
}

type replicaSuggestionConfirmResp struct {
	Fingerprint string                      `json:"fingerprint"`
	Head        string                      `json:"head"`
	Members     []string                    `json:"members"`
	Roles       []replicaSuggestionRoleResp `json:"roles"`
}

// confirmSuggestionReq is the optional body of the confirm route. Without adopt
// it is ignored. With adopt, declaredSnapshot is the declared list the
// operator saw (the same entries as a suggestion's declared field); the write
// only happens if the declarations are still exactly that.
type confirmSuggestionReq struct {
	Adopt            bool                            `json:"adopt"`
	DeclaredSnapshot []replicaSuggestionDeclaredResp `json:"declaredSnapshot"`
}

// dismissSuggestionReq is the optional body of the dismiss route: the state the
// client rendered when the operator dismissed the suggestion.
type dismissSuggestionReq struct {
	State string `json:"state"`
}

func toSuggestionResp(s router.ReplicaSuggestion, dismissed bool) replicaSuggestionResp {
	out := replicaSuggestionResp{
		Fingerprint: s.Fingerprint,
		Launcher:    s.Launcher,
		Runtime:     s.Runtime,
		State:       s.State,
		Reason:      s.Reason,
		Head:        s.Head,
		Members:     append([]string{}, s.Members...),
		Evidence:    make([]replicaSuggestionEvidenceResp, 0, len(s.Evidence)),
		Missing:     append([]string{}, s.Missing...),
		Declared:    make([]replicaSuggestionDeclaredResp, 0, len(s.Declared)),
		Confirmable: s.Confirmable,
		Dismissed:   dismissed,
	}
	for _, e := range s.Evidence {
		out.Evidence = append(out.Evidence, replicaSuggestionEvidenceResp{Node: e.Node, Source: e.Source, Detail: e.Detail})
	}
	for _, d := range s.Declared {
		out.Declared = append(out.Declared, replicaSuggestionDeclaredResp{Node: d.Node, Members: append([]string{}, d.Members...), Head: d.Head})
	}
	return out
}

// isSuggestionFingerprint reports whether fp has the shape the correlator
// produces: 16 lowercase hex characters.
func isSuggestionFingerprint(fp string) bool {
	if len(fp) != 16 {
		return false
	}
	for _, c := range fp {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// decodeOptionalJSON decodes a request body into v; an empty body is fine and
// leaves v untouched.
func decodeOptionalJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxSuggestionBodyBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// loadDismissedSuggestions returns the stored dismissal entries, oldest first.
// A missing, empty or null saved value is an empty list. Any other failure
// (the read itself failing, or a saved value that is not a list of strings) is
// returned, never turned into an empty list: callers that write the list back
// would otherwise erase every other dismissal. Caller holds replicaSuggestMu
// when it intends to write back.
func (s *Server) loadDismissedSuggestions() ([]string, error) {
	raw, err := s.st.GetSetting(replicaSuggestionsDismissedKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", replicaSuggestionsDismissedKey, err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", replicaSuggestionsDismissedKey, err)
	}
	// A JSON null element decodes to an empty string, so an empty fingerprint
	// also catches [null] and entries with nothing before the state suffix.
	for i, e := range list {
		if fp, _ := splitDismissal(e); fp == "" {
			return nil, fmt.Errorf("parse %s: entry %d is empty", replicaSuggestionsDismissedKey, i)
		}
	}
	return list, nil
}

// failDismissedList logs why the dismissal list could not be read and answers
// with the operator-facing message.
func (s *Server) failDismissedList(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("admin: replica suggestions %s %s: %v", r.Method, r.URL.Path, err)
	writeJSONError(w, http.StatusInternalServerError, dismissedListUnreadableMsg)
}

// dismissedListUnreadableMsg is what a client sees when the saved dismissal
// list cannot be read. It names the setting so an operator with a corrupt
// value knows what to clear; the underlying error stays in the server log.
const dismissedListUnreadableMsg = "could not read the saved " + replicaSuggestionsDismissedKey +
	" setting; nothing was changed. If its stored value is corrupt, delete that row from the settings table in the marbor database and try again"

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// splitDismissal splits a stored entry on the first ':' into the fingerprint
// and the recorded state; a legacy plain entry has an empty state.
func splitDismissal(entry string) (fp, state string) {
	fp, state, _ = strings.Cut(entry, ":")
	return fp, state
}

// dismissedStates maps each dismissed fingerprint to the state recorded for it
// ("" for a legacy plain entry). A later entry for the same fingerprint wins.
func dismissedStates(list []string) map[string]string {
	out := make(map[string]string, len(list))
	for _, e := range list {
		fp, st := splitDismissal(e)
		out[fp] = st
	}
	return out
}

// isHidden reports whether a suggestion in currentState is hidden by the
// recorded dismissals: a legacy plain entry always hides it, a stateful one
// only while the state is the one that was dismissed.
func isHidden(dismissed map[string]string, fp, currentState string) bool {
	st, ok := dismissed[fp]
	return ok && (st == "" || st == currentState)
}

// GET /admin/replica-suggestions[?includeDismissed=true]
func (s *Server) handleReplicaSuggestions(w http.ResponseWriter, r *http.Request) {
	includeDismissed := r.URL.Query().Get("includeDismissed") == "true"

	s.replicaSuggestMu.Lock()
	list, err := s.loadDismissedSuggestions()
	s.replicaSuggestMu.Unlock()
	if err != nil {
		s.failDismissedList(w, r, err)
		return
	}
	dismissed := dismissedStates(list)

	sugg, cov := s.router.ReplicaSuggestions()
	resp := replicaSuggestionsListResp{
		Suggestions: make([]replicaSuggestionResp, 0, len(sugg)),
		Coverage:    make([]topologyCoverageResp, 0, len(cov)),
	}
	for _, sg := range sugg {
		isDismissed := isHidden(dismissed, sg.Fingerprint, sg.State)
		if isDismissed {
			resp.DismissedCount++
			if !includeDismissed {
				continue
			}
		}
		resp.Suggestions = append(resp.Suggestions, toSuggestionResp(sg, isDismissed))
	}
	for _, c := range cov {
		resp.Coverage = append(resp.Coverage, topologyCoverageResp{Node: c.Node, Host: c.Host, State: c.State, Detected: c.Detected, Detail: c.Detail})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// validateSuggestedReplica applies the same structural checks a node PATCH
// applies to a declared replica_peers value, to what confirm is about to write.
func validateSuggestedReplica(members []string, head string) error {
	if len(members) == 0 || head == "" {
		return errors.New("empty members or head")
	}
	seen := make(map[string]bool, len(members))
	headInMembers := false
	for _, m := range members {
		if m == "" || seen[m] {
			return errors.New("empty or duplicate member")
		}
		seen[m] = true
		if m == head {
			headInMembers = true
		}
	}
	if !headInMembers {
		return errors.New("head is not a member")
	}
	return nil
}

// POST /admin/replica-suggestions/{fingerprint}/confirm
//
// Recomputes the suggestions now, so what gets written is what the agents
// report at this moment, never what an earlier page showed. All or nothing: the
// store is written first inside one transaction and memory is set only after
// that succeeded.
//
// The optional body {"adopt":true,"declaredSnapshot":[...]} overwrites the
// declarations of a contradicts_declared group with the detected one; see
// checkAdopt for the refusals that guard it.
func (s *Server) handleConfirmReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fingerprint")
	if !isSuggestionFingerprint(fp) {
		writeJSONError(w, http.StatusBadRequest, "invalid suggestion fingerprint")
		return
	}
	var req confirmSuggestionReq
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var snapshot map[string]replicaSuggestionDeclaredResp
	if req.Adopt {
		var msg string
		if snapshot, msg = normalizeDeclaredSnapshot(req.DeclaredSnapshot); msg != "" {
			writeJSONError(w, http.StatusBadRequest, msg)
			return
		}
	}

	// Serialize with node PATCH and node removal, which also use this lock.
	s.nodePatchMu.Lock()
	defer s.nodePatchMu.Unlock()

	sugg, _ := s.router.ReplicaSuggestions()
	var matches []router.ReplicaSuggestion
	for _, sg := range sugg {
		if sg.Fingerprint == fp {
			matches = append(matches, sg)
		}
	}
	switch {
	case len(matches) == 0:
		writeJSONError(w, http.StatusNotFound, "no current suggestion has that fingerprint; it may have changed or already been applied")
		return
	case len(matches) > 1:
		writeJSONError(w, http.StatusConflict, "more than one suggestion shares that fingerprint; declare the replica on its nodes directly")
		return
	}
	sg := matches[0]

	var overwritten []string
	if req.Adopt {
		if overwritten = s.checkAdopt(w, sg, snapshot); overwritten == nil {
			return
		}
	} else {
		switch sg.State {
		case router.SuggestionComplete:
		case router.SuggestionContradictsDeclared:
			writeSuggestionConflict(w, "a member already declares a different replica membership; clear or fix that declaration first", sg)
			return
		default:
			writeJSONError(w, http.StatusBadRequest, "this suggestion is not complete and cannot be confirmed")
			return
		}
	}

	members := append([]string(nil), sg.Members...)
	sort.Strings(members)
	if err := validateSuggestedReplica(members, sg.Head); err != nil {
		log.Printf("admin: confirm replica suggestion %s: refusing malformed suggestion: %v", fp, err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	assign := make(map[string]store.ReplicaPeers, len(members))
	for _, m := range members {
		assign[m] = store.ReplicaPeers{Members: append([]string(nil), members...), Head: sg.Head}
	}

	if err := s.router.ApplyReplicaPeersBatch(assign, s.st.SetReplicaPeersBatch); err != nil {
		if errors.Is(err, router.ErrReplicaMemberMissing) || errors.Is(err, store.ErrNodeNotRegistered) {
			writeJSONError(w, http.StatusConflict, "a member of this group is no longer registered; refresh and review it again")
			return
		}
		log.Printf("admin: confirm replica suggestion %s: %v", fp, err)
		writeJSONError(w, http.StatusInternalServerError, "could not save the replica declaration; nothing was changed")
		return
	}

	detail := fmt.Sprintf("Members: %s, Head: %s, Launcher: %s", strings.Join(members, ","), sg.Head, sg.Launcher)
	if req.Adopt {
		detail += ", Overwrote declarations on: " + strings.Join(overwritten, ",")
	}
	s.logSystemChange(r, "confirm_replica_suggestion", sg.Head, detail)

	nodes := s.router.Nodes()
	roles, heads := s.router.SchedulingRolesWithHeads(nodes)
	resp := replicaSuggestionConfirmResp{Fingerprint: fp, Head: sg.Head, Members: members, Roles: make([]replicaSuggestionRoleResp, 0, len(members))}
	for _, m := range members {
		resp.Roles = append(resp.Roles, replicaSuggestionRoleResp{Node: m, Role: roles[m].String(), Head: heads[m]})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// maxDeclaredSnapshotEntries bounds the snapshot an adopt request may carry. A
// real group declares one entry per member, far below this.
const maxDeclaredSnapshotEntries = 256

// normalizeDeclaredSnapshot turns the snapshot a client sent into a map keyed by
// node with members deduplicated and sorted. It returns the message for the 400
// the caller should answer when the snapshot is missing or malformed.
func normalizeDeclaredSnapshot(in []replicaSuggestionDeclaredResp) (map[string]replicaSuggestionDeclaredResp, string) {
	if len(in) == 0 {
		return nil, "adopt requires declaredSnapshot: the declared entries the suggestion showed"
	}
	if len(in) > maxDeclaredSnapshotEntries {
		return nil, fmt.Sprintf("declaredSnapshot has too many entries (limit %d)", maxDeclaredSnapshotEntries)
	}
	out := make(map[string]replicaSuggestionDeclaredResp, len(in))
	for _, d := range in {
		if d.Node == "" {
			return nil, "declaredSnapshot has an entry without a node"
		}
		if _, dup := out[d.Node]; dup {
			return nil, "declaredSnapshot lists node " + d.Node + " more than once (duplicate node)"
		}
		out[d.Node] = replicaSuggestionDeclaredResp{Node: d.Node, Head: d.Head, Members: dedupeSortedStrings(d.Members)}
	}
	return out, ""
}

func dedupeSortedStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// checkAdopt runs every refusal for an adopt request against the freshly
// computed suggestion and writes the 409 itself. It returns the sorted names of
// the nodes whose declaration will be overwritten, or nil when it refused. The
// caller holds nodePatchMu, so the snapshot check, the scan for declarations
// outside the group and the write that follows see one consistent state.
func (s *Server) checkAdopt(w http.ResponseWriter, sg router.ReplicaSuggestion, snapshot map[string]replicaSuggestionDeclaredResp) []string {
	switch sg.State {
	case router.SuggestionContradictsDeclared:
	case router.SuggestionComplete:
		writeSuggestionConflict(w, "this group no longer conflicts with a declaration and is ready to confirm; confirm it without adopting", sg)
		return nil
	default:
		writeSuggestionConflict(w, "this suggestion is "+sg.State+" and cannot be adopted", sg)
		return nil
	}

	same := len(sg.Declared) == len(snapshot)
	for _, d := range sg.Declared {
		snap, ok := snapshot[d.Node]
		if !ok || snap.Head != d.Head || !equalStringSlices(snap.Members, dedupeSortedStrings(d.Members)) {
			same = false
		}
	}
	if !same {
		writeSuggestionConflict(w, "the declarations on this group changed since you reviewed it; review the current state and try again", sg)
		return nil
	}

	inGroup := make(map[string]bool, len(sg.Members))
	for _, m := range sg.Members {
		inGroup[m] = true
	}
	var outside []string
	for _, n := range s.router.Nodes() {
		if inGroup[n.Name] {
			continue
		}
		n.RLock()
		rp := n.ReplicaPeers
		names := rp != nil && (inGroup[rp.Head] || anyInSet(rp.Members, inGroup))
		n.RUnlock()
		if names {
			outside = append(outside, n.Name)
		}
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		writeSuggestionConflict(w, "these nodes outside the group declare a replica membership that includes a member of it: "+
			strings.Join(outside, ", ")+"; fix or clear their declarations first", sg)
		return nil
	}

	names := make([]string, 0, len(sg.Declared))
	for _, d := range sg.Declared {
		names = append(names, d.Node)
	}
	sort.Strings(names)
	return names
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func anyInSet(list []string, set map[string]bool) bool {
	for _, v := range list {
		if set[v] {
			return true
		}
	}
	return false
}

// writeSuggestionConflict answers 409 with the fresh suggestion so a client can
// show the current state instead of the one it acted on.
func writeSuggestionConflict(w http.ResponseWriter, msg string, sg router.ReplicaSuggestion) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      msg,
		"suggestion": toSuggestionResp(sg, false),
	})
}

// POST /admin/replica-suggestions/{fingerprint}/dismiss
//
// The optional body {"state":"..."} is the state the client rendered; without
// it the suggestion's current state is recorded, or none if the fingerprint is
// not current.
func (s *Server) handleDismissReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	s.setSuggestionDismissed(w, r, true)
}

// DELETE /admin/replica-suggestions/{fingerprint}/dismiss
func (s *Server) handleRestoreReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	s.setSuggestionDismissed(w, r, false)
}

// setSuggestionDismissed hides or restores a suggestion. Idempotent, and it
// only changes visibility: a dismissed suggestion can still be confirmed. A
// dismissal remembers the state the suggestion was in, so it comes back when
// that state changes.
func (s *Server) setSuggestionDismissed(w http.ResponseWriter, r *http.Request, dismiss bool) {
	fp := r.PathValue("fingerprint")
	if !isSuggestionFingerprint(fp) {
		writeJSONError(w, http.StatusBadRequest, "invalid suggestion fingerprint")
		return
	}

	entry := fp
	if dismiss {
		var req dismissSuggestionReq
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		state := req.State
		if state != "" && !knownSuggestionStates[state] {
			writeJSONError(w, http.StatusBadRequest, "unknown suggestion state")
			return
		}
		if state == "" {
			// Computed outside nodePatchMu on purpose: dismiss only changes
			// visibility, so a state that moves in between is harmless.
			sugg, _ := s.router.ReplicaSuggestions()
			for _, sg := range sugg {
				if sg.Fingerprint == fp {
					state = sg.State
					break
				}
			}
		}
		// A fingerprint that is not currently suggested leaves state empty and
		// stores a plain legacy entry, which hides it in any state. That is by
		// design and matches the documented no-body behavior.
		if state != "" {
			entry = fp + ":" + state
		}
	}

	s.replicaSuggestMu.Lock()
	defer s.replicaSuggestMu.Unlock()
	list, err := s.loadDismissedSuggestions()
	if err != nil {
		s.failDismissedList(w, r, err)
		return
	}
	next := make([]string, 0, len(list)+1)
	for _, x := range list {
		if entryFP, _ := splitDismissal(x); entryFP != fp {
			next = append(next, x)
		}
	}
	if dismiss {
		next = append(next, entry)
		if len(next) > maxDismissedSuggestions {
			next = next[len(next)-maxDismissedSuggestions:]
		}
	}
	if dismiss || len(next) != len(list) {
		if err := store.SetJSONSetting(s.st, replicaSuggestionsDismissedKey, next); err != nil {
			log.Printf("admin: replica suggestions %s %s: persist dismissed list: %v", r.Method, r.URL.Path, err)
			writeJSONError(w, http.StatusInternalServerError, "could not save the dismissal")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"fingerprint": fp, "dismissed": dismiss})
}
