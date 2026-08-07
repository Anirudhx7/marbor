package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// TestAdmin_AddKeySetsLocalOnly guards that POST /admin/keys persists the
// P66 local_only field, the same way daily_usd_cap/monthly_usd_cap already
// do (see TestAdmin_AddKeyResponseContainsPlaintext above for that pattern).
func TestAdmin_AddKeySetsLocalOnly(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	a := auth.NewMiddleware(config.AuthConfig{})
	tmpDB := filepath.Join(t.TempDir(), "addkey.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(r, a, config.Config{}, st)

	// config.KeyConfig JSON tags are camelCase (matches its "rateLimit",
	// "dailyUsdCap" siblings) - handleAddKey decodes directly into KeyConfig,
	// unlike handlePatchKey which decodes into auth.KeyPatch (snake_case).
	body := bytes.NewReader([]byte(`{"name":"finance","rateLimit":100,"localOnly":true}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", body)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rec.Code, rec.Body.String())
	}
	if !a.IsLocalOnly("finance") {
		t.Error("expected auth middleware to have local_only=true for finance immediately after add")
	}

	keys, err := st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.Name == "finance" {
			found = true
			if !k.LocalOnly {
				t.Error("expected persisted KeyRecord.LocalOnly=true")
			}
		}
	}
	if !found {
		t.Fatal("finance key not found in store after add")
	}
}

// TestAdmin_PatchKeyLocalOnly guards that PATCH /admin/keys/{name} can
// toggle local_only after creation, and that GET /admin/keys reflects the
// live (patched) value - mirroring TestAdmin_PatchKeyExpiresAt above.
func TestAdmin_PatchKeyLocalOnly(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	a := auth.NewMiddleware(config.AuthConfig{})
	s := NewServer(r, a, config.Config{})
	a.AddKey(config.KeyConfig{Name: "k1", Key: "sk-1", RateLimit: 1000})

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/admin/keys/k1", bytes.NewReader([]byte(body)))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := patch(`{"local_only":true}`); rec.Code != http.StatusOK {
		t.Fatalf("patching local_only=true: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !a.IsLocalOnly("k1") {
		t.Error("expected IsLocalOnly=true after patch")
	}

	if rec := patch(`{"local_only":false}`); rec.Code != http.StatusOK {
		t.Fatalf("patching local_only=false: status = %d, want 200", rec.Code)
	}
	if a.IsLocalOnly("k1") {
		t.Error("expected IsLocalOnly=false after clearing the patch")
	}
}

// TestAdmin_HandleSpillCounters guards the GET /admin/spill read surface:
// it must return every (key_name, served_by) row fleet-wide as a flat JSON
// array, matching the documented response shape.
func TestAdmin_HandleSpillCounters(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	tmpDB := filepath.Join(t.TempDir(), "spill.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(r, nil, config.Config{}, st)

	s.IncrSpill("finance", "local")
	s.IncrSpill("finance", "local")
	s.IncrSpill("finance", "openai")
	s.IncrSpill("finance", "blocked")

	req := httptest.NewRequest(http.MethodGet, "/admin/spill", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var rows []store.SpillCounterRow
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode spill counters: %v", err)
	}
	got := map[string]int64{}
	for _, row := range rows {
		if row.KeyName != "finance" {
			t.Fatalf("unexpected key_name in response: %+v", row)
		}
		got[row.ServedBy] = row.Requests
	}
	want := map[string]int64{"local": 2, "openai": 1, "blocked": 1}
	for servedBy, wantCount := range want {
		if got[servedBy] != wantCount {
			t.Errorf("served_by=%s: got %d, want %d (rows: %+v)", servedBy, got[servedBy], wantCount, rows)
		}
	}
}

// TestAdmin_HandleSpillCountersEmpty guards that an empty spill_counters
// table renders as [] rather than null, so UI/CLI JSON consumers never need
// a null-check.
func TestAdmin_HandleSpillCountersEmpty(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/spill", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q, want %q", got, "[]\n")
	}
}
