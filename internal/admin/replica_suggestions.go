package admin

// replica_suggestions.go - the Admin API for replica suggestions: list what the
// agents' multi-host launch evidence suggests, confirm one (declare the same
// replica_peers on every member in one all-or-nothing step), and dismiss or
// restore a suggestion. Detection never writes anything by itself; only
// confirm does, and it writes ordinary declared replica_peers that the
// operator can clear again like any other declaration.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	// replicaSuggestionsDismissedKey holds the dismissed fingerprints as a
	// JSON string array in the settings table, oldest first.
	replicaSuggestionsDismissedKey = "replica_suggestions_dismissed"
	// maxDismissedSuggestions caps how many dismissals are remembered; the
	// oldest is forgotten first.
	maxDismissedSuggestions = 100
)

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

// loadDismissedSuggestions returns the dismissed fingerprints, oldest first.
// Caller holds replicaSuggestMu when it intends to write back.
func (s *Server) loadDismissedSuggestions() []string {
	var list []string
	store.GetJSONSetting(s.st, replicaSuggestionsDismissedKey, &list)
	return list
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// GET /admin/replica-suggestions[?includeDismissed=true]
func (s *Server) handleReplicaSuggestions(w http.ResponseWriter, r *http.Request) {
	includeDismissed := r.URL.Query().Get("includeDismissed") == "true"

	s.replicaSuggestMu.Lock()
	dismissed := s.loadDismissedSuggestions()
	s.replicaSuggestMu.Unlock()

	sugg, cov := s.router.ReplicaSuggestions()
	resp := replicaSuggestionsListResp{
		Suggestions: make([]replicaSuggestionResp, 0, len(sugg)),
		Coverage:    make([]topologyCoverageResp, 0, len(cov)),
	}
	for _, sg := range sugg {
		isDismissed := containsString(dismissed, sg.Fingerprint)
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
func (s *Server) handleConfirmReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fingerprint")
	if !isSuggestionFingerprint(fp) {
		writeJSONError(w, http.StatusBadRequest, "invalid suggestion fingerprint")
		return
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
	switch sg.State {
	case router.SuggestionComplete:
	case router.SuggestionContradictsDeclared:
		writeSuggestionConflict(w, "a member already declares a different replica membership; clear or fix that declaration first", sg)
		return
	default:
		writeJSONError(w, http.StatusBadRequest, "this suggestion is not complete and cannot be confirmed")
		return
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

	s.logSystemChange(r, "confirm_replica_suggestion", sg.Head,
		fmt.Sprintf("Members: %s, Head: %s, Launcher: %s", strings.Join(members, ","), sg.Head, sg.Launcher))

	nodes := s.router.Nodes()
	roles, heads := s.router.SchedulingRolesWithHeads(nodes)
	resp := replicaSuggestionConfirmResp{Fingerprint: fp, Head: sg.Head, Members: members, Roles: make([]replicaSuggestionRoleResp, 0, len(members))}
	for _, m := range members {
		resp.Roles = append(resp.Roles, replicaSuggestionRoleResp{Node: m, Role: roles[m].String(), Head: heads[m]})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
func (s *Server) handleDismissReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	s.setSuggestionDismissed(w, r, true)
}

// DELETE /admin/replica-suggestions/{fingerprint}/dismiss
func (s *Server) handleRestoreReplicaSuggestion(w http.ResponseWriter, r *http.Request) {
	s.setSuggestionDismissed(w, r, false)
}

// setSuggestionDismissed hides or restores a suggestion. Idempotent, and it
// only changes visibility: a dismissed suggestion can still be confirmed.
func (s *Server) setSuggestionDismissed(w http.ResponseWriter, r *http.Request, dismiss bool) {
	fp := r.PathValue("fingerprint")
	if !isSuggestionFingerprint(fp) {
		writeJSONError(w, http.StatusBadRequest, "invalid suggestion fingerprint")
		return
	}

	s.replicaSuggestMu.Lock()
	defer s.replicaSuggestMu.Unlock()
	list := s.loadDismissedSuggestions()
	has := containsString(list, fp)
	if dismiss != has {
		next := make([]string, 0, len(list)+1)
		for _, x := range list {
			if x != fp {
				next = append(next, x)
			}
		}
		if dismiss {
			next = append(next, fp)
			if len(next) > maxDismissedSuggestions {
				next = next[len(next)-maxDismissedSuggestions:]
			}
		}
		if err := store.SetJSONSetting(s.st, replicaSuggestionsDismissedKey, next); err != nil {
			log.Printf("admin: persist dismissed replica suggestions: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "could not save the dismissal")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"fingerprint": fp, "dismissed": dismiss})
}
