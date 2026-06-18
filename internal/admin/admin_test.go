package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newTestServer() *Server {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{})
}

func TestTrackLocalRequestModel(t *testing.T) {
	s := newTestServer()

	s.TrackLocalRequestModel("llama3", 100)
	s.TrackLocalRequestModel("llama3", 200)
	s.TrackLocalRequestModel("llama3", 0) // token count unavailable

	if got := atomic.LoadInt64(&s.localCount); got != 3 {
		t.Errorf("localCount = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&s.localTokens); got != 300 {
		t.Errorf("localTokens = %d, want 300", got)
	}
}

func TestTrackCloudCostModel(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCostModel("gpt-4o", 0.002, 1000)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)

	if gotCount := atomic.LoadInt64(&s.cloudCount); gotCount != 2 {
		t.Errorf("cloudCount = %d, want 2", gotCount)
	}

	// 0.002 * 1000/1000 + 0.002 * 500/1000 = 0.003 USD
	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()

	wantSpent := 0.002*1000.0/1000.0 + 0.002*500.0/1000.0
	if gotSpent != wantSpent {
		t.Errorf("cloudSpentUSD = %f, want %f", gotSpent, wantSpent)
	}
}

func TestTrackCloudCostModelUnknownTokensAddsNoCost(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCostModel("gpt-4o", 0.002, 0)

	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()
	if gotSpent != 0 {
		t.Errorf("cloudSpentUSD = %f, want 0 (no fabricated cost)", gotSpent)
	}
}

func TestHandleSavings(t *testing.T) {
	s := newTestServer()

	// 3 local requests totaling 1500 tokens, 2 cloud at $0.002/1K tokens
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackLocalRequestModel("llama3", 500)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)

	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil)
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	rec := httptest.NewRecorder()

	s.handleSavings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, field := range []string{"local_requests", "cloud_requests", "cloud_spent_usd", "saved_usd"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("response missing field %q", field)
		}
	}

	if got := resp["local_requests"].(float64); got != 3 {
		t.Errorf("local_requests = %v, want 3", got)
	}
	if got := resp["cloud_requests"].(float64); got != 2 {
		t.Errorf("cloud_requests = %v, want 2", got)
	}

	// cloud_spent_usd: 2 * (0.002 * 500/1000) = 0.002
	wantSpent := 0.002 * 500.0 / 1000.0 * 2
	if got := resp["cloud_spent_usd"].(float64); got != wantSpent {
		t.Errorf("cloud_spent_usd = %v, want %v", got, wantSpent)
	}

	// saved_usd: 1500 tokens * $0.002/1K = 0.003
	wantSaved := 1500.0 / 1000.0 * 0.002
	if got := resp["saved_usd"].(float64); got != wantSaved {
		t.Errorf("saved_usd = %v, want %v", got, wantSaved)
	}
}

func TestHandleSavingsCustomReferenceRate(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{Savings: config.SavingsConfig{ReferenceCostPer1K: 0.01}}
	s := NewServer(r, nil, cfg)

	s.TrackLocalRequestModel("llama3", 1500)

	rec := httptest.NewRecorder()
	s.handleSavings(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil))

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// saved_usd: 1500 tokens / 1000 * $0.01 = 0.015
	wantSaved := 1500.0 / 1000.0 * 0.01
	if got := resp["saved_usd"].(float64); got != wantSaved {
		t.Errorf("saved_usd = %v, want %v (custom reference rate must flow from config)", got, wantSaved)
	}

	// The hourly analytics buckets must use the same configured rate.
	var bucketSaved float64
	for _, b := range s.analytics.last24hBuckets() {
		bucketSaved += b.SavedUSD
	}
	if bucketSaved != wantSaved {
		t.Errorf("analytics SavedUSD = %v, want %v (analyticsStore must use configured rate)", bucketSaved, wantSaved)
	}
}

func TestHandleSavingsNullWhenNoTokenData(t *testing.T) {
	s := newTestServer()

	// Requests happened but no token counts could be parsed.
	s.TrackLocalRequestModel("llama3", 0)
	s.TrackCloudCostModel("gpt-4o", 0.002, 0)

	rec := httptest.NewRecorder()
	s.handleSavings(rec, httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil))

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if v, ok := resp["saved_usd"]; !ok || v != nil {
		t.Errorf("saved_usd = %v, want null when no token data", v)
	}
	if v, ok := resp["cloud_spent_usd"]; !ok || v != nil {
		t.Errorf("cloud_spent_usd = %v, want null when no token data", v)
	}
}

// TestAdmin_SettingsExcludesSecrets verifies that GET /admin/settings never
// returns the admin token value or cloud provider API keys in the response body.
func TestAdmin_SettingsExcludesSecrets(t *testing.T) {
	adminToken := "super-secret-admin-token-xyz"
	cloudAPIKey := "sk-openai-should-not-appear"

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{
		Auth: config.AuthConfig{
			AdminToken: adminToken,
		},
		CloudProviders: []config.CloudProvider{
			{
				Name:    "openai",
				APIKey:  cloudAPIKey,
				Enabled: true,
			},
		},
	}
	s := NewServer(r, nil, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	s.handleSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, adminToken) {
		t.Errorf("/admin/settings response contains admin token %q; it must be excluded", adminToken)
	}
	if strings.Contains(body, cloudAPIKey) {
		t.Errorf("/admin/settings response contains cloud API key %q; it must be masked", cloudAPIKey)
	}
}

// TestAdmin_KeysNeverPlaintext verifies that GET /admin/v1/keys never returns
// the full plaintext API key value; only a masked preview is allowed.
func TestAdmin_KeysNeverPlaintext(t *testing.T) {
	const fullKey = "sk-prod-abcdef1234567890abcdef1234567890abcdef12"

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled: true,
			Keys: []config.KeyConfig{
				{Name: "prod", Key: fullKey, RateLimit: 100},
			},
		},
	}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, fullKey) {
		t.Errorf("/admin/v1/keys response contains full plaintext key %q; only masked preview is permitted", fullKey)
	}

	// Confirm the response actually contains a key entry (so this is not a vacuous test).
	var keys []keyResp
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&keys); err != nil {
		t.Fatalf("decode keys response: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("keys response is empty; expected at least one key entry")
	}
	// The masked preview must be non-empty so callers can identify the key.
	if keys[0].Key == "" {
		t.Error("key.Key is empty; expected a masked preview (e.g. sk-prod…1234)")
	}
}

// TestAdmin_AddKeyResponseContainsPlaintext verifies that POST /admin/keys
// (the creation endpoint) returns the full key once — this is the only time it
// appears in an API response.
func TestAdmin_AddKeyResponseContainsPlaintext(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	body := bytes.NewReader([]byte(`{"name":"newkey","rate_limit":100}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", body)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	var k config.KeyConfig
	if err := json.NewDecoder(rec.Body).Decode(&k); err != nil {
		t.Fatalf("decode add-key response: %v", err)
	}
	if k.Key == "" {
		t.Error("add-key response is missing the plaintext key; it should be returned once at creation")
	}
	// Now verify that a subsequent GET /admin/keys does NOT contain this key.
	req2 := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req2.Header.Set("Authorization", "Bearer "+s.AdminToken())
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)

	if strings.Contains(rec2.Body.String(), k.Key) {
		t.Errorf("GET /admin/keys contains plaintext key %q after creation; only masked preview permitted", k.Key)
	}
}

// TestNewServerGeneratesRandomAdminToken verifies that a server built from an
// empty config (no admin_token, no auth keys) gets a non-empty, randomly
// generated token instead of the old guessable "admin" literal.
func TestNewServerGeneratesRandomAdminToken(t *testing.T) {
	s := newTestServer()
	tok := s.AdminToken()
	if tok == "" {
		t.Fatal("AdminToken is empty; expected a generated token")
	}
	if tok == "admin" {
		t.Fatal(`AdminToken is the guessable literal "admin"; expected a random token`)
	}
	if len(tok) < 32 {
		t.Errorf("AdminToken length = %d, want >= 32 (32 random bytes hex-encoded)", len(tok))
	}
	// A second server must get a different token (proves randomness, not a constant).
	if other := newTestServer().AdminToken(); other == tok {
		t.Error("two servers produced identical admin tokens; expected random per-server tokens")
	}
}
