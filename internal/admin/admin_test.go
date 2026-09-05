package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func newTestServer() *Server {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{})
}

func TestTrackLocalRequestModel(t *testing.T) {
	s := newTestServer()

	s.TrackLocalRequestModel("testkey", "llama3", 100, 0)
	s.TrackLocalRequestModel("testkey", "llama3", 200, 0)
	s.TrackLocalRequestModel("testkey", "llama3", 0, 0) // token count unavailable

	if got := atomic.LoadInt64(&s.localCount); got != 3 {
		t.Errorf("localCount = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&s.localTokens); got != 300 {
		t.Errorf("localTokens = %d, want 300", got)
	}
}

func TestTrackLocalRequestModelComputesTokensPerSec(t *testing.T) {
	s := newTestServer()

	// 100 tokens in 2000ms, then 100 more in 2000ms: 200 tokens / 4s = 50 tok/s.
	s.TrackLocalRequestModel("testkey", "llama3", 100, 2000)
	s.TrackLocalRequestModel("testkey", "llama3", 100, 2000)

	buckets := s.analytics.last24hBuckets()
	last := buckets[len(buckets)-1]
	if last.TokensPerSec != 50 {
		t.Errorf("TokensPerSec = %v, want 50", last.TokensPerSec)
	}
}

func TestTrackLocalRequestModelZeroDurationExcludedFromTPS(t *testing.T) {
	s := newTestServer()

	// Cloud-shaped response with tokens but no real generation duration must
	// not be divided by zero or rendered as a fabricated rate.
	s.TrackLocalRequestModel("testkey", "llama3", 100, 0)

	buckets := s.analytics.last24hBuckets()
	last := buckets[len(buckets)-1]
	if last.TokensPerSec != 0 {
		t.Errorf("TokensPerSec = %v, want 0 when GenDurationMs is 0", last.TokensPerSec)
	}
}

func TestTrackCloudCostModel(t *testing.T) {
	s := newTestServer()

	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 1000)
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 500)

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

	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 0)

	s.mu.RLock()
	gotSpent := s.cloudSpentUSD
	s.mu.RUnlock()
	if gotSpent != 0 {
		t.Errorf("cloudSpentUSD = %f, want 0 (no fabricated cost)", gotSpent)
	}
}

// TestLoadFromStoreRestoresAnalytics verifies that LoadFromStore backfills
// the in-memory hourly analytics buckets from SQLite on startup, so the
// dashboard's traffic chart shows continuous history immediately after a
// restart instead of a gap (docs/LIMITATIONS.md "Analytics dashboard shows a
// gap after restart").
func TestLoadFromStoreRestoresAnalytics(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "analytics-restore.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Write two hourly buckets directly to the store, simulating traffic
	// recorded by a prior process before a restart.
	hourA := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	hourB := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Hour)
	if err := st.UpsertHourlyBucket(store.HourlyBucket{
		Hour:          hourA,
		LocalRequests: 5,
		CloudRequests: 2,
		Tokens:        1000,
		CostUSD:       0.004,
	}); err != nil {
		t.Fatalf("UpsertHourlyBucket A: %v", err)
	}
	if err := st.UpsertHourlyBucket(store.HourlyBucket{
		Hour:          hourB,
		LocalRequests: 3,
		CloudRequests: 1,
		Tokens:        500,
		CostUSD:       0.002,
	}); err != nil {
		t.Fatalf("UpsertHourlyBucket B: %v", err)
	}

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	// Sanity check: before restore, the in-memory store is cold.
	for _, b := range s.analytics.last24hBuckets() {
		if b.Local != 0 || b.Cloud != 0 {
			t.Fatalf("expected cold analytics store before LoadFromStore, got %+v", b)
		}
	}

	if err := s.LoadFromStore(); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	buckets := s.analytics.last24hBuckets()
	var gotA, gotB *HourlyBucket
	keyA := hourA.Format("2006-01-02T15")
	keyB := hourB.Format("2006-01-02T15")
	for i := range buckets {
		switch buckets[i].Hour {
		case keyA:
			gotA = &buckets[i]
		case keyB:
			gotB = &buckets[i]
		}
	}

	if gotA == nil {
		t.Fatalf("hour %s not found in restored buckets", keyA)
	}
	if gotA.Local != 5 || gotA.Cloud != 2 || gotA.SpentUSD != 0.004 {
		t.Errorf("bucket A = %+v, want Local=5 Cloud=2 SpentUSD=0.004", gotA)
	}

	if gotB == nil {
		t.Fatalf("hour %s not found in restored buckets", keyB)
	}
	if gotB.Local != 3 || gotB.Cloud != 1 || gotB.SpentUSD != 0.002 {
		t.Errorf("bucket B = %+v, want Local=3 Cloud=1 SpentUSD=0.002", gotB)
	}
}

func TestHandleSavings(t *testing.T) {
	s := newTestServer()

	// 3 local requests totaling 1500 tokens, 2 cloud at $0.002/1K tokens
	s.TrackLocalRequestModel("testkey", "llama3", 500, 0)
	s.TrackLocalRequestModel("testkey", "llama3", 500, 0)
	s.TrackLocalRequestModel("testkey", "llama3", 500, 0)
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 500)
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 500)

	req := httptest.NewRequest(http.MethodGet, "/admin/metrics/savings", nil)
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

	s.TrackLocalRequestModel("testkey", "llama3", 1500, 0)

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
	s.TrackLocalRequestModel("testkey", "llama3", 0, 0)
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 0.002, 0)

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
	cloudAPIKey := "sk-openai-should-not-appear"

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{
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
	rec := httptest.NewRecorder()
	s.handleSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
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
			Enabled: config.BoolPtr(true),
			Keys: []config.KeyConfig{
				{Name: "prod", Key: fullKey, RateLimit: 100},
			},
		},
	}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
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
// (the creation endpoint) returns the full key once - this is the only time it
// appears in an API response.
func TestAdmin_AddKeyResponseContainsPlaintext(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	cfg := config.Config{}
	a := auth.NewMiddleware(cfg.Auth)
	s := NewServer(r, a, cfg)

	body := bytes.NewReader([]byte(`{"name":"newkey","rate_limit":100}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", body)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
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
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)

	if strings.Contains(rec2.Body.String(), k.Key) {
		t.Errorf("GET /admin/keys contains plaintext key %q after creation; only masked preview permitted", k.Key)
	}
}

func TestValidateExpiresAt(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty is valid (no expiry)", "", false},
		{"future bare date", "2099-01-01", false},
		{"future datetime-local (UI picker format)", "2099-01-01T15:04", false},
		{"future RFC3339", "2099-01-01T15:04:00Z", false},
		{"past bare date", "2020-01-01", true},
		{"past datetime-local", "2020-01-01T15:04", true},
		{"malformed", "not-a-date", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExpiresAt(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateExpiresAt(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

// TestAdmin_PatchKeyExpiresAt is a regression test: expires_at could only be
// set at creation - PATCH /admin/keys/{name} silently ignored it because
// auth.KeyPatch had no field for it.
func TestAdmin_PatchKeyExpiresAt(t *testing.T) {
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

	if rec := patch(`{"expires_at":"2020-01-01"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("patching a past expires_at: status = %d, want 400", rec.Code)
	}
	if rec := patch(`{"expires_at":"2099-01-01"}`); rec.Code != http.StatusOK {
		t.Errorf("patching a future expires_at: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"expires_at":""}`); rec.Code != http.StatusOK {
		t.Errorf("clearing expires_at: status = %d, want 200", rec.Code)
	}
}

// TestShutdownDrainsAsyncLogQueue verifies that Shutdown() flushes any
// buffered request logs to the store before returning, so LogRequest calls
// made just before shutdown are not lost. Also verifies that a LogRequest
// call arriving after Shutdown() has returned does not panic (logChan is
// never closed, so a late send is dropped, not a crash) - this is the
// scenario that motivated the fix: the store used to be closed out from
// under the async logger goroutine.
func TestShutdownDrainsAsyncLogQueue(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "shutdown-drain.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	for i := 0; i < 10; i++ {
		s.LogRequest(fmt.Sprintf("req-%d", i), "key1", "127.0.0.1", "llama3", "node1", "200", 200, 12, 100, nil)
	}

	s.Shutdown()

	recs, err := st.LastRequests(10)
	if err != nil {
		t.Fatalf("LastRequests: %v", err)
	}
	if len(recs) != 10 {
		t.Errorf("LastRequests returned %d records after Shutdown, want 10 (queue should be fully drained)", len(recs))
	}

	// A LogRequest call after Shutdown must not panic (send on closed
	// channel) even though the async logger has already exited.
	s.LogRequest("req-after-shutdown", "key1", "127.0.0.1", "llama3", "node1", "200", 200, 12, 100, nil)
}

// newScheduleTestServer builds an admin Server with one registered node
// ("n1"), for exercising the /admin/schedules handlers end to end.
func newScheduleTestServer() *Server {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: "http://127.0.0.1:0"}}, nil)
	cfg := config.Config{}
	a := auth.NewMiddleware(cfg.Auth)
	return NewServer(r, a, cfg)
}

func doScheduleRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandleCreateScheduleRejectsUnknownNode verifies a schedule against a
// node name that isn't registered is rejected at creation time (400) instead
// of being silently accepted and then no-oping every time it fires.
func TestHandleCreateScheduleRejectsUnknownNode(t *testing.T) {
	s := newScheduleTestServer()
	rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"drain","node":"ghost","at":"09:00","enabled":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(s.router.Schedules()) != 0 {
		t.Error("schedule against an unknown node should not have been persisted")
	}
}

// TestHandleCreateScheduleRequiresModelsForWarmupUnload verifies a warmup or
// unload schedule with no models selected is rejected - such a schedule would
// otherwise fire "successfully" every tick while its models loop runs zero
// times, i.e. it would never actually warm up or unload anything.
func TestHandleCreateScheduleRequiresModelsForWarmupUnload(t *testing.T) {
	s := newScheduleTestServer()
	for _, action := range []string{"warmup", "unload"} {
		rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"`+action+`","node":"n1","at":"09:00","enabled":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("action=%s: status = %d, want 400; body=%s", action, rec.Code, rec.Body.String())
		}
	}
	if len(s.router.Schedules()) != 0 {
		t.Error("model-less warmup/unload schedules should not have been persisted")
	}
}

// TestHandleCreateScheduleSucceedsForValidNode is the positive control for
// the two rejection tests above: a schedule against a real node, with models
// where required, is accepted and persisted.
func TestHandleCreateScheduleSucceedsForValidNode(t *testing.T) {
	s := newScheduleTestServer()
	rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"drain","node":"n1","at":"09:00","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	rec = doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"warmup","node":"n1","models":["llama3"],"at":"09:30","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(s.router.Schedules()); got != 2 {
		t.Errorf("Schedules() len = %d, want 2", got)
	}
}

// TestHandlePatchScheduleRejectsUnknownNode verifies editing a schedule to
// point at an unregistered node is rejected, mirroring the create-time check.
func TestHandlePatchScheduleRejectsUnknownNode(t *testing.T) {
	s := newScheduleTestServer()
	rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"drain","node":"n1","at":"09:00","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	rec = doScheduleRequest(s, http.MethodPatch, "/admin/schedules/"+created.ID, `{"node":"ghost"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	scheds := s.router.Schedules()
	if len(scheds) != 1 || scheds[0].Node != "n1" {
		t.Errorf("schedule node should remain unchanged after a rejected patch, got %+v", scheds)
	}
}

// TestHandlePatchScheduleUnknownNodeErrorBodyIsValidJSON is a regression test
// for a bug where the "node not registered" error body was built with
// fmt.Sprintf(`{"error":"node %q is not registered"}`, name): %q already
// wraps its argument in its own literal quote characters, so splicing it into
// a template that is itself already inside a JSON string produced invalid
// JSON (the embedded quotes prematurely closed the JSON string value). The
// frontend's res.json() would throw on that body and silently fall back to a
// generic "Failed to update schedule" message instead of surfacing the real,
// actionable error. This asserts the body is valid JSON with the expected
// "error" field so that regression can't reappear silently.
func TestHandlePatchScheduleUnknownNodeErrorBodyIsValidJSON(t *testing.T) {
	s := newScheduleTestServer()
	rec := doScheduleRequest(s, http.MethodPost, "/admin/schedules", `{"action":"drain","node":"n1","at":"09:00","enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	rec = doScheduleRequest(s, http.MethodPatch, "/admin/schedules/"+created.ID, `{"node":"pve"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("error response body is not valid JSON: %v; body=%q", err, rec.Body.String())
	}
	if !strings.Contains(parsed.Error, "pve") || !strings.Contains(parsed.Error, "not registered") {
		t.Errorf("error = %q, want it to mention the unregistered node %q", parsed.Error, "pve")
	}
}

// TestHandleAddNode_RejectsDuplicateURL verifies the admin "add node" API
// (POST /admin/nodes) refuses to register a URL that already belongs to a
// different, existing node instead of silently creating a second live
// NodeState for the same physical backend (see Router.AddNode /
// FindNodeByURL). This is the admin-API-facing half of the fix; the
// router-level check itself is covered by
// TestAddNode_RejectsDuplicateURLUnderDifferentName in internal/router.
func TestHandleAddNode_RejectsDuplicateURL(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "pve", URL: "http://192.168.1.115:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"name":"discovered-ollama-1","url":"http://192.168.1.115:11434/"}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes", body)
	rec := httptest.NewRecorder()
	s.handleAddNode(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a URL that duplicates existing node %q", rec.Code, "pve")
	}
	if got := len(r.Nodes()); got != 1 {
		t.Fatalf("router has %d nodes after rejected duplicate add, want 1", got)
	}
}

// TestHandleAddNode_UpsertsSameNameInsteadOfDuplicating is the HTTP-handler
// regression test for the confirmed bug: POST /admin/nodes twice with the
// identical {name, url} must not silently double the router's live node
// count (SQLite's UpsertNode was always idempotent by name via INSERT OR
// REPLACE; Router.AddNode previously was not - see Router.AddNode for the
// full history). The second call is an update, so it must return 200 OK,
// not 201 Created, to honestly reflect "updated" rather than "created".
func TestHandleAddNode_UpsertsSameNameInsteadOfDuplicating(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	s := NewServer(r, nil, config.Config{})

	body := `{"name":"gpu-01","url":"http://192.168.1.50:11434"}`

	rec := httptest.NewRecorder()
	s.handleAddNode(rec, httptest.NewRequest(http.MethodPost, "/admin/nodes", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add: status = %d, want 201", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleAddNode(rec, httptest.NewRequest(http.MethodPost, "/admin/nodes", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("second (repeat) add: status = %d, want 200 (update, not create)", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleAddNode(rec, httptest.NewRequest(http.MethodPost, "/admin/nodes", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("third (repeat) add: status = %d, want 200 (update, not create)", rec.Code)
	}

	if got := len(r.Nodes()); got != 1 {
		t.Fatalf("router has %d live nodes after 3x identical POST /admin/nodes, want 1", got)
	}
}

// TestHandleAddNode_UpsertReplacesURL verifies the deliberate-upsert design:
// re-POSTing an existing node name with a different url updates that node's
// url in place rather than being rejected or creating a second node - this
// matches the DB layer's pre-existing unconditional-replace behavior, not a
// new restriction (see Router.AddNode).
func TestHandleAddNode_UpsertReplacesURL(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	s := NewServer(r, nil, config.Config{})

	rec := httptest.NewRecorder()
	s.handleAddNode(rec, httptest.NewRequest(http.MethodPost, "/admin/nodes", bytes.NewReader([]byte(`{"name":"gpu-01","url":"http://192.168.1.50:11434"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add: status = %d, want 201", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleAddNode(rec, httptest.NewRequest(http.MethodPost, "/admin/nodes", bytes.NewReader([]byte(`{"name":"gpu-01","url":"http://192.168.1.51:11434"}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert with new url: status = %d, want 200", rec.Code)
	}

	nodes := r.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes after upsert, want 1", len(nodes))
	}
	if nodes[0].URL != "http://192.168.1.51:11434" {
		t.Errorf("URL = %q, want upserted URL", nodes[0].URL)
	}
}

// TestHandleAddNode_AllowsNewURL is the negative case: a genuinely new URL
// must still be accepted and registered normally.
func TestHandleAddNode_AllowsNewURL(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "pve", URL: "http://192.168.1.115:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"name":"other-box","url":"http://192.168.1.116:11434"}`))
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes", body)
	rec := httptest.NewRecorder()
	s.handleAddNode(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a distinct URL", rec.Code)
	}
	if got := len(r.Nodes()); got != 2 {
		t.Fatalf("router has %d nodes after adding a distinct URL, want 2", got)
	}
}

// TestHandlePatchNode_RejectsUnknownRuntime verifies a PATCH with an
// unrecognized runtime value is rejected with 400 before it ever reaches
// router.PatchNode or the store override.
func TestHandlePatchNode_RejectsUnknownRuntime(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"runtime":"lmstudio"}`))
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", body)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown runtime", rec.Code)
	}
}

// TestHandlePatchNode_RejectsInvalidMaxInFlight verifies a negative or
// int32-overflowing max_in_flight is rejected with 400 before it reaches
// router.PatchNode or the store override - either value would otherwise be
// read by isUnderCapacity as "uncapped" (negative) or "permanently over
// capacity" (overflow wraps negative in the int32 cast), inverting the
// operator's intent silently instead of erroring.
func TestHandlePatchNode_RejectsInvalidMaxInFlight(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	for _, body := range []string{`{"max_in_flight":-1}`, `{"max_in_flight":2147483648}`} {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", bytes.NewReader([]byte(body)))
		req.SetPathValue("name", "gpu-0")
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestHandlePatchNode_SetsMaxInFlight verifies a valid max_in_flight patch is
// applied to the live node, mirroring TestHandlePatchNode_SetsRuntime.
func TestHandlePatchNode_SetsMaxInFlight(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"max_in_flight":4}`))
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", body)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	nodes := r.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	nodes[0].RLock()
	got := nodes[0].MaxInFlight
	nodes[0].RUnlock()
	if got != 4 {
		t.Errorf("MaxInFlight = %d, want 4", got)
	}
}

// TestHandlePatchNode_SetsRuntime verifies a valid runtime patch is applied
// to the live node.
func TestHandlePatchNode_SetsRuntime(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "ollama"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"runtime":"vllm"}`))
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", body)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	nodes := r.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	nodes[0].RLock()
	got := nodes[0].Runtime
	nodes[0].RUnlock()
	if got != "vllm" {
		t.Errorf("Runtime = %q, want vllm", got)
	}
}

// TestHandlePatchNode_RejectsNonPositiveVRAMOverride verifies a 0 or negative
// vram_overrides value is rejected with 400 before it reaches router.PatchNode
// or the store override - such a value would otherwise be silently treated
// as "no override" downstream (estimateModelSizeBytes' map lookup), never
// actually applying, which R1 requires rejecting explicitly rather than
// accepting silently.
func TestHandlePatchNode_RejectsNonPositiveVRAMOverride(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	for _, body := range []string{`{"vram_overrides":{"llama3.1:8b":0}}`, `{"vram_overrides":{"llama3.1:8b":-1}}`} {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", bytes.NewReader([]byte(body)))
		req.SetPathValue("name", "gpu-0")
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestHandlePatchNode_SetsVRAMOverrides verifies the VRAM-override admin API wiring:
// PATCH /admin/nodes/{name} accepts "vram_overrides" and applies it to the
// live node, mirroring TestHandlePatchNode_SetsGPUIndices.
func TestHandlePatchNode_SetsVRAMOverrides(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"vram_overrides":{"llama3.1:8b":8192}}`))
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", body)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	nodes := r.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	nodes[0].RLock()
	got := nodes[0].VRAMOverrides["llama3.1:8b"]
	nodes[0].RUnlock()
	if got != 8192 {
		t.Errorf("VRAMOverrides[llama3.1:8b] = %d, want 8192", got)
	}
}

// TestHandlePatchNode_SetsGPUIndices verifies the declared-GPU-indices admin API
// wiring: PATCH /admin/nodes/{name} accepts "gpu_indices" and applies it to
// the live NodeState via router.PatchNode, mirroring TestHandlePatchNode_SetsRuntime.
func TestHandlePatchNode_SetsGPUIndices(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	body := bytes.NewReader([]byte(`{"gpu_indices":[0,1]}`))
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/gpu-0", body)
	req.SetPathValue("name", "gpu-0")
	rec := httptest.NewRecorder()
	s.handlePatchNode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	nodes := r.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	nodes[0].RLock()
	got := append([]int(nil), nodes[0].DeclaredGPUIndices...)
	nodes[0].RUnlock()
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("DeclaredGPUIndices = %v, want [0 1]", got)
	}
}

func TestCloudBudgetExceeded_DisabledByDefault(t *testing.T) {
	s := newTestServer()
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 1000.0, 1000) // huge cost, caps still 0/disabled

	if exceeded, reason := s.CloudBudgetExceeded(""); exceeded {
		t.Errorf("CloudBudgetExceeded = true (%q), want false when both caps are 0", reason)
	}
}

func TestCloudBudgetExceeded_DailyCapReached(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "cloud-budget-daily.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{
		CloudBudget: config.CloudBudgetConfig{DailyUSDCap: 1.0},
	}, st)

	if exceeded, _ := s.CloudBudgetExceeded(""); exceeded {
		t.Fatal("CloudBudgetExceeded = true before any spend")
	}

	// costPer1K * tokens/1000 = 2.0 * 1000/1000 = $1.00, hits the $1.00 cap.
	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 2.0, 1000)

	exceeded, reason := s.CloudBudgetExceeded("")
	if !exceeded {
		t.Fatal("CloudBudgetExceeded = false after spend reached the daily cap")
	}
	if reason == "" {
		t.Error("expected a non-empty reason when the daily cap is exceeded")
	}
}

func TestCloudBudgetExceeded_MonthlyCapReached(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "cloud-budget-monthly.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{
		CloudBudget: config.CloudBudgetConfig{MonthlyUSDCap: 0.5},
	}, st)

	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 1.0, 1000) // $1.00 spent, over the $0.50 monthly cap

	exceeded, reason := s.CloudBudgetExceeded("")
	if !exceeded {
		t.Fatal("CloudBudgetExceeded = false after spend reached the monthly cap")
	}
	if reason == "" {
		t.Error("expected a non-empty reason when the monthly cap is exceeded")
	}
}

func TestCloudBudgetExceeded_PerKeyDailyCap(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "cloud-budget-perkey.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mw := auth.NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "capped", Key: "sk-capped", DailyUsdCap: 1.0},
		},
	})

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, mw, config.Config{}, st)

	if exceeded, _ := s.CloudBudgetExceeded("capped"); exceeded {
		t.Fatal("CloudBudgetExceeded = true before any per-key spend")
	}

	// A different key's spend must never count against "capped".
	if err := st.AppendRequest(store.RequestRecord{ID: "r-other", KeyName: "other", CostUSD: 5.0, IsCloud: true, TS: time.Now()}); err != nil {
		t.Fatalf("AppendRequest: %v", err)
	}
	if exceeded, _ := s.CloudBudgetExceeded("capped"); exceeded {
		t.Fatal("CloudBudgetExceeded = true after a different key's spend, want false")
	}

	// "capped" spends $1.00 - $0.50 over half an hour ago, $0.50 just now - both within today (UTC).
	if err := st.AppendRequest(store.RequestRecord{ID: "r1", KeyName: "capped", CostUSD: 0.5, IsCloud: true, TS: time.Now()}); err != nil {
		t.Fatalf("AppendRequest: %v", err)
	}
	if err := st.AppendRequest(store.RequestRecord{ID: "r2", KeyName: "capped", CostUSD: 0.5, IsCloud: true, TS: time.Now()}); err != nil {
		t.Fatalf("AppendRequest: %v", err)
	}

	exceeded, reason := s.CloudBudgetExceeded("capped")
	if !exceeded {
		t.Fatal("CloudBudgetExceeded = false after per-key spend reached the daily cap")
	}
	if reason == "" {
		t.Error("expected a non-empty reason when the per-key daily cap is exceeded")
	}

	// An uncapped key must never be blocked regardless of spend.
	if exceeded, reason := s.CloudBudgetExceeded("uncapped-key"); exceeded {
		t.Errorf("CloudBudgetExceeded(uncapped-key) = true (%q), want false", reason)
	}
}

func TestHandlePredictiveDecisions_ReturnsRecordedDecisions(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	r.RecordTransition("model-a", time.Now())
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/predictive/decisions", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Decisions []map[string]interface{} `json:"decisions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Decisions == nil {
		t.Error("expected a decisions field (possibly empty), got nil")
	}
}

func TestContextWindowFor_UndeclaredModelHasNoCheck(t *testing.T) {
	s := newTestServer()

	if window, ok := s.ContextWindowFor("llama3.2:8b"); ok {
		t.Errorf("ContextWindowFor(undeclared) = (%d, true), want ok=false", window)
	}
}

func TestContextWindowFor_DeclaredModelReturnsWindow(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{
		ContextWindows: map[string]int{"llama3.2:8b": 8192},
	})

	window, ok := s.ContextWindowFor("llama3.2:8b")
	if !ok {
		t.Fatal("ContextWindowFor(declared model) ok = false, want true")
	}
	if window != 8192 {
		t.Errorf("ContextWindowFor window = %d, want 8192", window)
	}
}

func TestCloudBudgetExceeded_UnderCapAllowsFallback(t *testing.T) {
	tmpDB := filepath.Join(t.TempDir(), "cloud-budget-undercap.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{
		CloudBudget: config.CloudBudgetConfig{DailyUSDCap: 100.0},
	}, st)

	s.TrackCloudCostModel("testkey", "openai", "gpt-4o", 1.0, 1000) // $0.001, well under the $100 cap

	if exceeded, reason := s.CloudBudgetExceeded(""); exceeded {
		t.Errorf("CloudBudgetExceeded = true (%q), want false when spend is under the cap", reason)
	}
}

// --- Model configuration overrides: (model, node)-keyed admin API ---

func newModelConfigTestServer(t *testing.T) *Server {
	t.Helper()
	tmpDB := filepath.Join(t.TempDir(), "model-config.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{}, st)
}

// TestHandleSetGetDeleteModelConfig_RoundTrip verifies the full PUT/GET/DELETE
// cycle at the HTTP handler layer, keyed by (model, node) rather than model
// alone.
func TestHandleSetGetDeleteModelConfig_RoundTrip(t *testing.T) {
	s := newModelConfigTestServer(t)

	body := `{"model":"llama3.3:8b","node":"gpu-node-01","num_ctx":8192,"temperature":0.5}`
	putReq := httptest.NewRequest(http.MethodPut, "/admin/model-config", strings.NewReader(body))
	putRec := httptest.NewRecorder()
	s.handleSetModelConfig(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/model-config?model=llama3.3:8b&node=gpu-node-01", nil)
	getRec := httptest.NewRecorder()
	s.handleGetModelConfig(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var got store.ModelConfig
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if got.Node != "gpu-node-01" || got.NumCtx == nil || *got.NumCtx != 8192 {
		t.Fatalf("GET result = %+v, want node=gpu-node-01 num_ctx=8192", got)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/admin/model-config?model=llama3.3:8b&node=gpu-node-01", nil)
	delRec := httptest.NewRecorder()
	s.handleDeleteModelConfig(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", delRec.Code, delRec.Body.String())
	}

	getAfterDeleteReq := httptest.NewRequest(http.MethodGet, "/admin/model-config?model=llama3.3:8b&node=gpu-node-01", nil)
	getAfterDeleteRec := httptest.NewRecorder()
	s.handleGetModelConfig(getAfterDeleteRec, getAfterDeleteReq)
	if getAfterDeleteRec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want 404", getAfterDeleteRec.Code)
	}
}

// TestHandleGetModelConfig_RequiresNodeParam verifies the node query param is
// mandatory - a profile with no node has no meaning under the new keying.
func TestHandleGetModelConfig_RequiresNodeParam(t *testing.T) {
	s := newModelConfigTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/model-config?model=llama3.3:8b", nil)
	rec := httptest.NewRecorder()
	s.handleGetModelConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when node param is missing", rec.Code)
	}
}

// TestHandleSetModelConfig_RequiresNodeField verifies validateModelConfig
// rejects a profile with no node, not just no model.
func TestHandleSetModelConfig_RequiresNodeField(t *testing.T) {
	s := newModelConfigTestServer(t)
	body := `{"model":"llama3.3:8b","temperature":0.5}`
	req := httptest.NewRequest(http.MethodPut, "/admin/model-config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSetModelConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when node field is missing", rec.Code)
	}
}

// TestHandleModelConfig_SameModelDifferentNodesIndependent verifies the same
// model name can carry two independent profiles on two nodes, and deleting
// one doesn't touch the other - exercised at the HTTP layer this time, not
// just the store layer.
func TestHandleModelConfig_SameModelDifferentNodesIndependent(t *testing.T) {
	s := newModelConfigTestServer(t)

	for _, body := range []string{
		`{"model":"llama3.3:8b","node":"gpu-node-01","num_ctx":4096}`,
		`{"model":"llama3.3:8b","node":"gpu-node-02","num_ctx":8192}`,
	} {
		req := httptest.NewRequest(http.MethodPut, "/admin/model-config", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleSetModelConfig(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	listReq := httptest.NewRequest(http.MethodGet, "/admin/model-configs", nil)
	listRec := httptest.NewRecorder()
	s.handleListModelConfigs(listRec, listReq)
	var listResp struct {
		Configs []store.ModelConfig `json:"configs"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Configs) != 2 {
		t.Fatalf("model-configs list = %+v, want 2 entries for the same model on different nodes", listResp.Configs)
	}
}

// TestHandleModelConfigCapabilities verifies the capabilities endpoint
// returns Ollama's full field set, and correctly withholds Ollama-only
// load-time fields (num_ctx) from every other runtime, while giving vLLM
// and llama.cpp their own extra sampling fields beyond TGI's strict set.
func TestHandleModelConfigCapabilities(t *testing.T) {
	s := newModelConfigTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/model-config/capabilities", nil)
	rec := httptest.NewRecorder()
	s.handleModelConfigCapabilities(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var caps map[string][]string
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}

	contains := func(fields []string, want string) bool {
		for _, f := range fields {
			if f == want {
				return true
			}
		}
		return false
	}

	if !contains(caps["ollama"], "num_ctx") {
		t.Errorf("ollama capabilities missing num_ctx: %v", caps["ollama"])
	}
	if contains(caps["tgi"], "num_ctx") {
		t.Errorf("tgi capabilities should not include num_ctx (load-time, Ollama-only): %v", caps["tgi"])
	}
	if contains(caps["vllm"], "num_ctx") {
		t.Errorf("vllm capabilities should not include num_ctx (load-time, Ollama-only): %v", caps["vllm"])
	}
	if !contains(caps["vllm"], "top_k") {
		t.Errorf("vllm capabilities missing its top_k extra: %v", caps["vllm"])
	}
	if contains(caps["tgi"], "top_k") {
		t.Errorf("tgi capabilities should not include top_k (unsupported extra): %v", caps["tgi"])
	}
	if !contains(caps["llamacpp"], "mirostat") {
		t.Errorf("llamacpp capabilities missing its mirostat extra: %v", caps["llamacpp"])
	}
	if _, ok := caps["mlx"]; !ok {
		t.Errorf("capabilities response missing mlx runtime entry: %v", caps)
	}
	if contains(caps["mlx"], "num_ctx") {
		t.Errorf("mlx capabilities should not include num_ctx (load-time, Ollama-only): %v", caps["mlx"])
	}
	if !contains(caps["mlx"], "temperature") {
		t.Errorf("mlx capabilities missing base OpenAI-compat field temperature: %v", caps["mlx"])
	}
}

// TestHandleExplainRequest covers the routing-explain endpoint: 200 with the full
// RoutingDecision for a logged request, the routingReason surfacing on the
// list endpoint too, and 404 for an unknown id.
func TestHandleExplainRequest(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{})

	decision := &router.RoutingDecision{
		Node:   "node-a",
		Reason: router.ReasonScoreBased,
		Detail: "score_based on node node-a",
		Score:  42.5,
		Components: []router.ScoreComponent{
			{Name: "warm_model_resident", Raw: 0, Weight: 50, Value: 0},
			{Name: "free_vram_headroom", Raw: 1, Weight: 20, Value: 20},
		},
	}
	s.LogRequest("req-explain-1", "key1", "127.0.0.1", "llama3", "node-a", "warm", 200, 12, 100, decision)

	doGet := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("200 with full breakdown", func(t *testing.T) {
		rec := doGet("/admin/requests/req-explain-1/explain")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		var got router.RoutingDecision
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if got.Reason != router.ReasonScoreBased || got.Node != "node-a" || got.Score != 42.5 {
			t.Errorf("got %+v, want reason=score_based node=node-a score=42.5", got)
		}
		if len(got.Components) != 2 {
			t.Errorf("got %d components, want 2", len(got.Components))
		}
	})

	t.Run("admin/v1 alias also works", func(t *testing.T) {
		rec := doGet("/admin/v1/requests/req-explain-1/explain")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 for unknown id", func(t *testing.T) {
		rec := doGet("/admin/requests/does-not-exist/explain")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list endpoint carries routingReason", func(t *testing.T) {
		rec := doGet("/admin/requests")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var entries []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		found := false
		for _, e := range entries {
			if e["id"] == "req-explain-1" {
				found = true
				if e["routingReason"] != router.ReasonScoreBased {
					t.Errorf("routingReason = %v, want %q", e["routingReason"], router.ReasonScoreBased)
				}
			}
		}
		if !found {
			t.Fatal("logged request not found in /admin/requests list")
		}
	})
}

// TestHandleAuditRoutingReason verifies routing_reason survives the real
// production path: audit.Logger.Log -> async write -> SQLite audit_log ->
// QueryAuditLog -> handleAudit's JSON response (a code-review fix: this is
// the endpoint Requests.tsx actually reads for its list, not /admin/requests).
func TestHandleAuditRoutingReason(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)

	al := audit.New(st, true)
	s.SetAuditLogger(al)

	al.Log(audit.Entry{
		Time:          time.Now().UTC(),
		RequestID:     "req-audit-1",
		KeyName:       "key1",
		Model:         "llama3",
		Node:          "node-a",
		Status:        "200",
		LatencyMs:     12,
		Cloud:         false,
		RoutingReason: router.ReasonSessionAffinity,
	})
	al.Close() // synchronizes: guarantees the async write above landed in SQLite

	req := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	found := false
	for _, e := range resp.Entries {
		if e["request_id"] == "req-audit-1" {
			found = true
			if e["routing_reason"] != router.ReasonSessionAffinity {
				t.Errorf("routing_reason = %v, want %q", e["routing_reason"], router.ReasonSessionAffinity)
			}
		}
	}
	if !found {
		t.Fatal("logged entry not found in /admin/audit response")
	}
}
