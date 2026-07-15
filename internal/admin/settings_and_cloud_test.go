package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// newRealStoreTestServer builds a Server backed by a real (temp-file) SQLite
// store, needed for tests that verify settings/cloud-provider persistence -
// NopStore silently no-ops every write.
func newRealStoreTestServer(t *testing.T) *Server {
	t.Helper()
	tmpDB := filepath.Join(t.TempDir(), "settings-test.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st)
}

// TestUpdateSettings_PersistsNewConfigYAMLEliminationFields verifies that the
// ~20 fields added when config.yaml was eliminated (2026-07) actually persist
// to SQLite via PUT /admin/settings, covering both scalar and JSON-encoded
// (list/map) settings keys - the two persistence patterns main.go's boot
// overlay expects.
func TestUpdateSettings_PersistsNewConfigYAMLEliminationFields(t *testing.T) {
	s := newRealStoreTestServer(t)

	payload := config.Config{
		Admin: config.AdminConfig{BindAddress: "127.0.0.1:9999", CORSOrigin: "https://example.com"},
		Routing: config.RoutingConfig{
			Fallback:                 "round-robin",
			UpstreamTimeoutMs:        60000,
			MaxRetries:               5,
			AllowManagementEndpoints: true,
			SessionAffinity:          true,
			SessionAffinityTTL:       "20m",
			ThermalWatchdog:          config.ThermalWatchdogConfig{Enabled: true, MaxTempCelsius: 85, ConsecutiveBreaches: 4},
			FallbackChains:           map[string][]string{"llama3:70b": {"llama3:8b"}},
		},
		Docker:  config.DockerConfig{Enabled: true, Socket: "/tmp/docker.sock", PollIntervalMs: 15000},
		Audit:   config.AuditConfig{Enabled: true},
		Webhook: config.WebhookConfig{Enabled: true, URL: "https://hooks.example.com", Secret: "whsec-real"},
		Savings: config.SavingsConfig{ReferenceCostPer1K: 0.05},
		HA: config.HAConfig{
			Enabled: true, Peers: []string{"http://peer-a:8080", "http://peer-b:8080"},
			HeartbeatIntervalMs: 4000, PeerTimeoutMs: 2000,
		},
		Warmup: config.WarmupConfig{
			Enabled: true, IntervalMs: 60000, KeepAlive: "15m",
			Models: []config.WarmupEntry{{Model: "llama3:8b"}},
		},
		ContextWindows: map[string]int{"llama3:8b": 8192},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleUpdateSettings status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	checks := map[string]string{
		"admin_bind_address":                            "127.0.0.1:9999",
		"admin_cors_origin":                             "https://example.com",
		"routing_fallback":                              "round-robin",
		"routing_upstream_timeout_ms":                   "60000",
		"routing_max_retries":                           "5",
		"routing_allow_management_endpoints":            "true",
		"routing_session_affinity":                      "true",
		"routing_session_affinity_ttl":                  "20m",
		"routing_thermal_watchdog_enabled":              "true",
		"routing_thermal_watchdog_consecutive_breaches": "4",
		"docker_enabled":                                "true",
		"docker_socket":                                 "/tmp/docker.sock",
		"docker_poll_interval_ms":                       "15000",
		"audit_enabled":                                 "true",
		"webhook_enabled":                               "true",
		"webhook_url":                                   "https://hooks.example.com",
		"webhook_secret":                                "whsec-real",
		"ha_enabled":                                    "true",
		"ha_heartbeat_interval_ms":                      "4000",
		"ha_peer_timeout_ms":                            "2000",
		"warmup_enabled":                                "true",
		"warmup_interval_ms":                            "60000",
		"warmup_keep_alive":                             "15m",
	}
	for key, want := range checks {
		got, err := s.st.GetSetting(key)
		if err != nil {
			t.Errorf("GetSetting(%q): %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("setting %q = %q, want %q", key, got, want)
		}
	}

	var gotPeers []string
	store.GetJSONSetting(s.st, "ha_peers", &gotPeers)
	if len(gotPeers) != 2 || gotPeers[0] != "http://peer-a:8080" {
		t.Errorf("ha_peers = %v, want 2 peers starting with peer-a", gotPeers)
	}

	var gotWindows map[string]int
	store.GetJSONSetting(s.st, "context_windows", &gotWindows)
	if gotWindows["llama3:8b"] != 8192 {
		t.Errorf("context_windows[llama3:8b] = %d, want 8192", gotWindows["llama3:8b"])
	}
}

// TestUpdateSettings_WebhookSecretMaskNotPersisted verifies that echoing back
// the masked "***" placeholder (an operator who didn't touch the secret
// field) does not overwrite the real stored webhook secret - mirrors the
// existing HuggingFace-token mask-preserve behavior.
func TestUpdateSettings_WebhookSecretMaskNotPersisted(t *testing.T) {
	s := newRealStoreTestServer(t)

	first := config.Config{Webhook: config.WebhookConfig{Enabled: true, URL: "https://hooks.example.com", Secret: "real-secret"}}
	body, _ := json.Marshal(first)
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("first update status = %d, want 200", rec.Code)
	}

	second := config.Config{Webhook: config.WebhookConfig{Enabled: true, URL: "https://hooks.example.com", Secret: "***"}}
	body2, _ := json.Marshal(second)
	rec2 := httptest.NewRecorder()
	s.handleUpdateSettings(rec2, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body2)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second update status = %d, want 200", rec2.Code)
	}

	got, err := s.st.GetSetting("webhook_secret")
	if err != nil {
		t.Fatalf("GetSetting(webhook_secret): %v", err)
	}
	if got != "real-secret" {
		t.Errorf("webhook_secret = %q after masked update, want unchanged %q", got, "real-secret")
	}
}

// TestLiteLLMAPIKeyMaskedOnGetAndPreservedOnUpdate verifies that GET
// /admin/settings masks a stored LiteLLM API key as "***" and that echoing
// that placeholder back on a subsequent update preserves the real key,
// mirroring the webhook secret and HuggingFace token mask-preserve behavior.
func TestLiteLLMAPIKeyMaskedOnGetAndPreservedOnUpdate(t *testing.T) {
	s := newRealStoreTestServer(t)

	first := config.Config{LiteLLM: config.LiteLLMConfig{Enabled: true, URL: "http://localhost:4000", APIKey: "sk-real"}}
	body, _ := json.Marshal(first)
	rec := httptest.NewRecorder()
	s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update settings status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	getRec := httptest.NewRecorder()
	s.handleSettings(getRec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get settings status = %d, want 200; body: %s", getRec.Code, getRec.Body.String())
	}
	var got config.Config
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if got.LiteLLM.APIKey != "***" {
		t.Errorf("GET litellm.api_key = %q, want masked ***", got.LiteLLM.APIKey)
	}

	// Send the update again with the masked placeholder - the real key must survive.
	second := config.Config{LiteLLM: config.LiteLLMConfig{Enabled: true, URL: "http://localhost:4000", APIKey: "***"}}
	body2, _ := json.Marshal(second)
	rec2 := httptest.NewRecorder()
	s.handleUpdateSettings(rec2, httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body2)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second update status = %d, want 200", rec2.Code)
	}
	if s.cfg.LiteLLM.APIKey != "sk-real" {
		t.Errorf("LiteLLM.APIKey after masked update = %q, want sk-real preserved", s.cfg.LiteLLM.APIKey)
	}

	got2, err := s.st.GetSetting("litellm_api_key")
	if err != nil {
		t.Fatalf("GetSetting(litellm_api_key): %v", err)
	}
	if got2 != "sk-real" {
		t.Errorf("persisted litellm_api_key = %q, want sk-real", got2)
	}
}

// TestCloudProviderCRUD exercises add/list/update/delete end to end through
// the real HTTP handlers, verifying both the SQLite persistence and that the
// masked list response never leaks the plaintext API key.
func TestCloudProviderCRUD(t *testing.T) {
	s := newRealStoreTestServer(t)

	addBody := `{"name":"openai-prod","provider":"openai","base_url":"https://api.openai.com/v1","api_key":"sk-real-key","enabled":true}`
	addRec := httptest.NewRecorder()
	s.handleAddCloudProvider(addRec, httptest.NewRequest(http.MethodPost, "/admin/cloud/providers", bytes.NewReader([]byte(addBody))))
	if addRec.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want 201; body: %s", addRec.Code, addRec.Body.String())
	}

	listRec := httptest.NewRecorder()
	s.handleCloudProviders(listRec, httptest.NewRequest(http.MethodGet, "/admin/cloud/providers", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listRec.Code)
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte("sk-real-key")) {
		t.Error("cloud providers list response leaks the plaintext API key")
	}
	var listed []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "openai-prod" || !listed[0].Enabled {
		t.Fatalf("listed providers = %+v, want one enabled openai-prod", listed)
	}

	// Update: disable it, omit api_key (must preserve the stored key).
	updateBody := `{"provider":"openai","base_url":"https://api.openai.com/v1","enabled":false}`
	updateReq := httptest.NewRequest(http.MethodPut, "/admin/cloud/providers/openai-prod", bytes.NewReader([]byte(updateBody)))
	updateReq.SetPathValue("name", "openai-prod")
	updateRec := httptest.NewRecorder()
	s.handleUpdateCloudProvider(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body: %s", updateRec.Code, updateRec.Body.String())
	}
	providers, err := s.st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: %v", err)
	}
	if len(providers) != 1 || providers[0].Enabled || providers[0].APIKey != "sk-real-key" {
		t.Fatalf("after update: providers = %+v, want disabled with preserved api key", providers)
	}

	// Delete.
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/cloud/providers/openai-prod", nil)
	delReq.SetPathValue("name", "openai-prod")
	delRec := httptest.NewRecorder()
	s.handleDeleteCloudProvider(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delRec.Code)
	}
	providers, err = s.st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders after delete: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers after delete = %+v, want empty", providers)
	}
}

// TestHandleReorderCloudProviders verifies that PUT
// /admin/cloud/providers/reorder renumbers the persisted providers to match
// the caller's desired order and re-syncs the live router so CloudChain()
// reflects the new order immediately.
func TestHandleReorderCloudProviders(t *testing.T) {
	s := newRealStoreTestServer(t)

	for _, name := range []string{"a", "b"} {
		body, _ := json.Marshal(map[string]interface{}{
			"name": name, "provider": "openai", "base_url": "https://api.openai.com/v1",
			"api_key": "sk-test", "enabled": true,
		})
		rec := httptest.NewRecorder()
		s.handleAddCloudProvider(rec, httptest.NewRequest(http.MethodPost, "/admin/cloud/providers", bytes.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed provider %s: status = %d, body=%s", name, rec.Code, rec.Body.String())
		}
	}

	reorderBody, _ := json.Marshal(map[string]interface{}{"order": []string{"b", "a"}})
	rec := httptest.NewRecorder()
	s.handleReorderCloudProviders(rec, httptest.NewRequest(http.MethodPut, "/admin/cloud/providers/reorder", bytes.NewReader(reorderBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder: status = %d, body=%s", rec.Code, rec.Body.String())
	}

	got := s.router.CloudChain()
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("CloudChain() after reorder = %v, want [b a]", got)
	}
}

// TestFreshStore_BootsWithZeroRowsWithoutPanicking verifies that a brand-new
// SQLite database (no config.yaml, no seeded rows - the 2026-07 blank-slate
// boot path) produces a fully-functional server: auth enabled by default,
// zero nodes/keys, and no panics constructing/serving from it.
func TestFreshStore_BootsWithZeroRowsWithoutPanicking(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "fresh-boot.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !cfg.Auth.IsEnabled() {
		t.Error("auth should default to enabled on a blank-slate config")
	}

	r := router.New(cfg.Routing, cfg.Nodes, cfg.CloudProviders)
	s := NewServer(r, nil, cfg, st)

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/nodes on fresh store status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var nodes []any
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("decode nodes response: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("fresh store nodes = %d, want 0", len(nodes))
	}
}
