package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// TestGenerateMarborAgentTokenEmbedsScope verifies generateMarborAgentToken (P54)
// produces a token whose scope round-trips through marboragent.TokenScope -
// the same parsing the agent binary itself uses to enforce per-route scope.
func TestGenerateMarborAgentTokenEmbedsScope(t *testing.T) {
	for _, scope := range []string{marboragent.ScopeReadonly, marboragent.ScopeOperator, marboragent.ScopeAdmin} {
		token, err := generateMarborAgentToken(scope)
		if err != nil {
			t.Fatalf("generateMarborAgentToken(%q): %v", scope, err)
		}
		if got := marboragent.TokenScope(token); got != scope {
			t.Errorf("TokenScope(generateMarborAgentToken(%q)) = %q, want %q", scope, got, scope)
		}
	}
}

// TestEnableMarborAgentPersistsAdminScope is the P54 admin-API regression:
// handleEnableMarborAgent must mint an admin-scope token (today's default -
// no Group 3 action exists yet to justify a lower tier) and persist that
// scope alongside the token, and it must round-trip through GetMarborAgent -
// the same record marbor reads back on every subsequent admin request.
func TestEnableMarborAgentPersistsAdminScope(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "marbor-agent-scope.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "test-node", URL: "http://127.0.0.1:11434", Runtime: "ollama"},
	}, nil)
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	s := NewServer(r, nil, cfg, st)

	body, _ := json.Marshal(map[string]int{"port": 9300})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/test-node/agent", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.SetPathValue("name", "test-node")
	w := httptest.NewRecorder()
	s.handleEnableMarborAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleEnableMarborAgent = %d, want 200: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := marboragent.TokenScope(resp.Token); got != marboragent.ScopeAdmin {
		t.Errorf("returned token scope = %q, want %q", got, marboragent.ScopeAdmin)
	}

	host, _ := r.NodeHost("test-node")
	rec, found, err := st.GetMarborAgent(host)
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found {
		t.Fatal("GetMarborAgent: record not found after enable")
	}
	if rec.Scope != marboragent.ScopeAdmin {
		t.Errorf("persisted MarborAgentRecord.Scope = %q, want %q", rec.Scope, marboragent.ScopeAdmin)
	}
	if rec.Token != resp.Token {
		t.Errorf("persisted token %q does not match returned token %q", rec.Token, resp.Token)
	}
}

// TestRegenerateMarborAgentTokenPersistsAdminScope mirrors the above for the
// regenerate path (handleRegenerateMarborAgentToken), which mints a fresh
// token independently of handleEnableMarborAgent's.
func TestRegenerateMarborAgentTokenPersistsAdminScope(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "marbor-agent-scope-regen.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "test-node", URL: "http://127.0.0.1:11434", Runtime: "ollama"},
	}, nil)
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "test", Key: "test-token"}},
		},
	}
	s := NewServer(r, nil, cfg, st)

	enableBody, _ := json.Marshal(map[string]int{"port": 9301})
	enableReq := httptest.NewRequest(http.MethodPost, "/admin/nodes/test-node/agent", bytes.NewReader(enableBody))
	enableReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	enableReq.SetPathValue("name", "test-node")
	enableW := httptest.NewRecorder()
	s.handleEnableMarborAgent(enableW, enableReq)
	if enableW.Code != http.StatusOK {
		t.Fatalf("handleEnableMarborAgent = %d, want 200: %s", enableW.Code, enableW.Body.String())
	}

	regenReq := httptest.NewRequest(http.MethodPost, "/admin/nodes/test-node/agent/regenerate", nil)
	regenReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	regenReq.SetPathValue("name", "test-node")
	regenW := httptest.NewRecorder()
	s.handleRegenerateMarborAgentToken(regenW, regenReq)
	if regenW.Code != http.StatusOK {
		t.Fatalf("handleRegenerateMarborAgentToken = %d, want 200: %s", regenW.Code, regenW.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(regenW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := marboragent.TokenScope(resp.Token); got != marboragent.ScopeAdmin {
		t.Errorf("regenerated token scope = %q, want %q", got, marboragent.ScopeAdmin)
	}

	host, _ := r.NodeHost("test-node")
	rec, found, err := st.GetMarborAgent(host)
	if err != nil {
		t.Fatalf("GetMarborAgent: %v", err)
	}
	if !found {
		t.Fatal("GetMarborAgent: record not found after regenerate")
	}
	if rec.Scope != marboragent.ScopeAdmin {
		t.Errorf("persisted MarborAgentRecord.Scope after regenerate = %q, want %q", rec.Scope, marboragent.ScopeAdmin)
	}
	if rec.Token != resp.Token {
		t.Errorf("persisted token %q does not match regenerated token %q", rec.Token, resp.Token)
	}
}
