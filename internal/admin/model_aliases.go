package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// modelAliasesSettingKey is the settings-table key holding the alias map as
// JSON (alias name -> real model). Loaded once at boot by main.go and written
// only by the handlers in this file, never by the settings PUT.
const modelAliasesSettingKey = "routing_model_aliases"

// modelAliasRow is one alias as the Admin API reports it. Every inventory
// field comes from the live fleet inventory (loaded models plus each healthy
// node's catalog). InventoryChecked is false when that inventory could not be
// read completely (no healthy node, or a catalog fetch failed); clients must
// then show TargetAvailable/ShadowsModel as "not checked", since a false
// there only means "not seen", not "confirmed absent".
type modelAliasRow struct {
	Alias            string `json:"alias"`
	Target           string `json:"target"`
	TargetAvailable  bool   `json:"target_available"`
	TargetStatus     string `json:"target_status,omitempty"`
	ShadowsModel     bool   `json:"shadows_model"`
	InventoryChecked bool   `json:"inventory_checked"`
}

func buildModelAliasRow(alias, target string, inventory map[string]string, complete bool) modelAliasRow {
	status := inventory[target]
	_, shadows := inventory[alias]
	return modelAliasRow{
		Alias:            alias,
		Target:           target,
		TargetAvailable:  status != "",
		TargetStatus:     status,
		ShadowsModel:     shadows,
		InventoryChecked: complete,
	}
}

// modelAliasProblem turns an alias validation result into the message sent
// back to the client (empty when valid). Alias validation errors are built
// only from the operator's own input and fixed rule text, never from store,
// file or network internals, so they are safe to return as-is - the same
// string-message shape validateModelConfig uses.
func modelAliasProblem(validationErr error) string {
	if validationErr == nil {
		return ""
	}
	return validationErr.Error()
}

// currentModelAliases returns a private copy of the live alias map.
func (s *Server) currentModelAliases() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.cfg.Routing.ModelAliases))
	for k, v := range s.cfg.Routing.ModelAliases {
		out[k] = v
	}
	return out
}

// commitModelAliases persists next, then publishes it to s.cfg and the
// router. The caller must hold s.settingsMu so the read-modify-write of the
// alias map is serialized with every other settings write.
func (s *Server) commitModelAliases(next map[string]string) error {
	if len(next) == 0 {
		next = nil
	}
	var persisted any = next
	if next == nil {
		persisted = map[string]string{}
	}
	if err := store.SetJSONSetting(s.st, modelAliasesSettingKey, persisted); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg.Routing.ModelAliases = next
	s.mu.Unlock()
	s.router.SetModelAliases(next)
	return nil
}

// handleListModelAliases lists every alias with its live inventory status.
// GET /admin/model-aliases.
func (s *Server) handleListModelAliases(w http.ResponseWriter, r *http.Request) {
	aliases := s.currentModelAliases()
	inventory, complete := s.router.FleetModelInventory()
	rows := make([]modelAliasRow, 0, len(aliases))
	for _, alias := range router.SortedModelAliasNames(aliases) {
		rows = append(rows, buildModelAliasRow(alias, aliases[alias], inventory, complete))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}

// handleSetModelAlias creates or replaces one alias. The alias name travels in
// the body, not the path, because model names routinely contain "/" and ":".
// PUT /admin/model-aliases, body {"alias": "gpt-4", "target": "llama3.2:8b"}.
func (s *Server) handleSetModelAlias(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var body struct {
		Alias  string `json:"alias"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	alias := strings.TrimSpace(body.Alias)
	target := strings.TrimSpace(body.Target)
	if msg := modelAliasProblem(config.ValidateModelAliasEntry(alias, target)); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := s.currentModelAliases()
	previous, replaced := next[alias]
	next[alias] = target
	if msg := modelAliasProblem(config.ValidateModelAliases(next)); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.commitModelAliases(next); err != nil {
		writeServerError(w, r, err)
		return
	}

	details := "target: " + target
	if replaced {
		details = fmt.Sprintf("target: %s (was %s)", target, previous)
	}
	s.logSystemChange(r, "set_model_alias", alias, details)

	inventory, complete := s.router.FleetModelInventory()
	row := buildModelAliasRow(alias, target, inventory, complete)
	if row.ShadowsModel {
		log.Printf("admin: model alias %q shadows a model currently on the fleet; requests for it now reach %q", alias, target)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(row)
}

// handleDeleteModelAlias removes one alias. Clients still requesting the
// alias name stop reaching its target immediately.
// DELETE /admin/model-aliases?alias=X.
func (s *Server) handleDeleteModelAlias(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.URL.Query().Get("alias"))
	if alias == "" {
		writeJSONError(w, http.StatusBadRequest, "alias query param is required")
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := s.currentModelAliases()
	target, ok := next[alias]
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("model alias %q not found", alias))
		return
	}
	delete(next, alias)
	if err := s.commitModelAliases(next); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.logSystemChange(r, "delete_model_alias", alias, "target was "+target)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"alias": alias, "target": target, "status": "removed"})
}
