package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// newAliasTestServer builds an admin server over a real SQLite store and a
// router with one healthy node whose catalog (/api/tags) lists catalog and
// whose loaded models are loaded.
func newAliasTestServer(t *testing.T, loaded []string, catalog []string) (*Server, store.Store, *router.Router) {
	t.Helper()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			type tag struct {
				Name string `json:"name"`
			}
			out := struct {
				Models []tag `json:"models"`
			}{}
			for _, c := range catalog {
				out.Models = append(out.Models, tag{Name: c})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(node.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "aliases.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: node.URL, Runtime: "ollama"}}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		for _, m := range loaded {
			n.LoadedModels = append(n.LoadedModels, router.ModelInfo{Name: m})
		}
		n.Unlock()
	}
	return NewServer(r, nil, config.Config{}, st), st, r
}

func aliasRequest(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func listAliases(t *testing.T, s *Server, path string) []modelAliasRow {
	t.Helper()
	rec := aliasRequest(t, s, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var rows []modelAliasRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return rows
}

func TestModelAliases_CRUD_PersistsAndHotReloads(t *testing.T) {
	s, st, r := newAliasTestServer(t, []string{"llama3.2:8b"}, []string{"llama3.2:8b", "qwen2.5:7b"})

	if rows := listAliases(t, s, "/admin/model-aliases"); len(rows) != 0 {
		t.Fatalf("initial aliases = %v, want none", rows)
	}

	rec := aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "gpt-4", "target": "llama3.2:8b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	var row modelAliasRow
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row.Alias != "gpt-4" || row.Target != "llama3.2:8b" || !row.TargetAvailable || row.TargetStatus != "loaded" || row.ShadowsModel || !row.InventoryChecked {
		t.Fatalf("PUT row = %+v", row)
	}

	// Hot reload: the router resolves immediately, no restart.
	if got, ok := r.ResolveModelAlias("gpt-4"); !ok || got != "llama3.2:8b" {
		t.Fatalf("router did not hot-reload the alias: %q, %v", got, ok)
	}
	// Persisted under the settings key main.go loads at boot.
	var stored map[string]string
	store.GetJSONSetting(st, "routing_model_aliases", &stored)
	if stored["gpt-4"] != "llama3.2:8b" {
		t.Fatalf("persisted aliases = %v", stored)
	}

	// Replace in place, and list through the versioned route too.
	if rec := aliasRequest(t, s, http.MethodPut, "/admin/v1/model-aliases", map[string]string{"alias": "gpt-4", "target": "qwen2.5:7b"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT replace = %d: %s", rec.Code, rec.Body.String())
	}
	rows := listAliases(t, s, "/admin/v1/model-aliases")
	if len(rows) != 1 || rows[0].Target != "qwen2.5:7b" || rows[0].TargetStatus != "available" {
		t.Fatalf("rows after replace = %+v", rows)
	}

	// Unknown target: listed, but never guessed available.
	aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "claude", "target": "not-on-fleet"})
	rows = listAliases(t, s, "/admin/model-aliases")
	if len(rows) != 2 || rows[0].Alias != "claude" || rows[0].TargetAvailable {
		t.Fatalf("rows = %+v, want claude first with target_available=false", rows)
	}

	// Delete.
	rec = aliasRequest(t, s, http.MethodDelete, "/admin/model-aliases?alias="+url.QueryEscape("gpt-4"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.ResolveModelAlias("gpt-4"); ok {
		t.Fatal("deleted alias still resolves")
	}
	stored = nil
	store.GetJSONSetting(st, "routing_model_aliases", &stored)
	if _, ok := stored["gpt-4"]; ok || stored["claude"] != "not-on-fleet" {
		t.Fatalf("persisted after delete = %v", stored)
	}
	if rec := aliasRequest(t, s, http.MethodDelete, "/admin/model-aliases?alias=gpt-4", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d, want 404", rec.Code)
	}
	if rec := aliasRequest(t, s, http.MethodDelete, "/admin/model-aliases", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE without alias = %d, want 400", rec.Code)
	}

	// Settings GET reflects the live value read-only.
	s.mu.RLock()
	live := s.cfg.Routing.ModelAliases["claude"]
	s.mu.RUnlock()
	if live != "not-on-fleet" {
		t.Fatalf("s.cfg aliases not updated: %q", live)
	}
}

func TestModelAliases_RejectsInvalid(t *testing.T) {
	s, _, r := newAliasTestServer(t, nil, nil)
	if rec := aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "a", "target": "b"}); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d", rec.Code)
	}
	cases := []map[string]string{
		{"alias": "", "target": "x"},
		{"alias": "x", "target": ""},
		{"alias": "x", "target": "x"},
		{"alias": "x\r\ny", "target": "z"},
		{"alias": "c", "target": "a"}, // target is an alias
		{"alias": "b", "target": "c"}, // alias name is another alias's target
	}
	for _, c := range cases {
		rec := aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %v = %d, want 400 (%s)", c, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/model-aliases", bytes.NewReader([]byte("{not json")))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rec.Code)
	}
	if got := r.ModelAliases(); len(got) != 1 || got["a"] != "b" {
		t.Fatalf("rejected writes must not change the live map: %v", got)
	}
}

func TestModelAliases_RequiresAdminAuth(t *testing.T) {
	s, _, _ := newAliasTestServer(t, nil, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/model-aliases"},
		{http.MethodPut, "/admin/model-aliases"},
		{http.MethodDelete, "/admin/model-aliases?alias=x"},
		{http.MethodGet, "/admin/v1/model-aliases"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{"alias":"x","target":"y"}`)))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without auth = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestUpdateSettings_PreservesModelAliases(t *testing.T) {
	s, st, r := newAliasTestServer(t, nil, nil)
	aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "gpt-4", "target": "llama3.2:8b"})

	// A client echoing back a STALE alias map (or trying to set one) through
	// the settings PUT must neither clobber nor bypass the alias endpoints -
	// including a map that would fail alias validation.
	payload := map[string]any{
		"routing": map[string]any{
			"model_aliases": map[string]string{"stale": "old-model", "loop": "loop"},
		},
	}
	b, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(b)))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings PUT = %d: %s", rec.Code, rec.Body.String())
	}

	s.mu.RLock()
	live := s.cfg.Routing.ModelAliases
	s.mu.RUnlock()
	if len(live) != 1 || live["gpt-4"] != "llama3.2:8b" {
		t.Fatalf("s.cfg aliases after settings PUT = %v", live)
	}
	if got := r.ModelAliases(); len(got) != 1 || got["gpt-4"] != "llama3.2:8b" {
		t.Fatalf("router aliases after settings PUT = %v", got)
	}
	var stored map[string]string
	store.GetJSONSetting(st, "routing_model_aliases", &stored)
	if len(stored) != 1 || stored["gpt-4"] != "llama3.2:8b" {
		t.Fatalf("stored aliases after settings PUT = %v", stored)
	}
}

func TestModelAliases_CollisionPolicy(t *testing.T) {
	// "mistral:7b" is a real model on the fleet; aliasing it anyway is
	// allowed (the alias always wins) but reported as shadowing.
	s, _, r := newAliasTestServer(t, []string{"mistral:7b"}, []string{"mistral:7b", "llama3.2:8b"})
	rec := aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "mistral:7b", "target": "llama3.2:8b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	var row modelAliasRow
	json.Unmarshal(rec.Body.Bytes(), &row)
	if !row.ShadowsModel || !row.InventoryChecked {
		t.Fatalf("PUT row = %+v, want shadows_model=true", row)
	}
	rows := listAliases(t, s, "/admin/model-aliases")
	if len(rows) != 1 || !rows[0].ShadowsModel {
		t.Fatalf("list rows = %+v, want shadows_model=true", rows)
	}
	// The alias wins at request time.
	if got, ok := r.ResolveModelAlias("mistral:7b"); !ok || got != "llama3.2:8b" {
		t.Fatalf("alias must win over the real model: %q, %v", got, ok)
	}
}

func TestModelAliases_InventoryNotChecked(t *testing.T) {
	// No healthy node: nothing can be confirmed, so every inventory flag is
	// false and inventory_checked tells clients to say "not checked".
	st, err := store.Open(filepath.Join(t.TempDir(), "aliases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := router.New(config.RoutingConfig{}, nil, nil)
	s := NewServer(r, nil, config.Config{}, st)
	rec := aliasRequest(t, s, http.MethodPut, "/admin/model-aliases", map[string]string{"alias": "gpt-4", "target": "llama3.2:8b"})
	var row modelAliasRow
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row.InventoryChecked || row.TargetAvailable || row.ShadowsModel {
		t.Fatalf("row = %+v, want every inventory flag false", row)
	}
}
