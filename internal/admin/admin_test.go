package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func newTestServer() *Server {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	return NewServer(r, nil, config.Config{})
}

func TestTrackLocalRequestModel(t *testing.T) {
	s := newTestServer()

	s.TrackLocalRequestModel("llama3", 100, 0)
	s.TrackLocalRequestModel("llama3", 200, 0)
	s.TrackLocalRequestModel("llama3", 0, 0) // token count unavailable

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
	s.TrackLocalRequestModel("llama3", 100, 2000)
	s.TrackLocalRequestModel("llama3", 100, 2000)

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
	s.TrackLocalRequestModel("llama3", 100, 0)

	buckets := s.analytics.last24hBuckets()
	last := buckets[len(buckets)-1]
	if last.TokensPerSec != 0 {
		t.Errorf("TokensPerSec = %v, want 0 when GenDurationMs is 0", last.TokensPerSec)
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
	s.TrackLocalRequestModel("llama3", 500, 0)
	s.TrackLocalRequestModel("llama3", 500, 0)
	s.TrackLocalRequestModel("llama3", 500, 0)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)
	s.TrackCloudCostModel("gpt-4o", 0.002, 500)

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

	s.TrackLocalRequestModel("llama3", 1500, 0)

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
	s.TrackLocalRequestModel("llama3", 0, 0)
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
			Enabled: config.BoolPtr(true),
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

// TestShutdownDrainsAsyncLogQueue verifies that Shutdown() flushes any
// buffered request logs to the store before returning, so LogRequest calls
// made just before shutdown are not lost. Also verifies that a LogRequest
// call arriving after Shutdown() has returned does not panic (logChan is
// never closed, so a late send is dropped, not a crash) — this is the
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
		s.LogRequest("key1", "127.0.0.1", "llama3", "node1", "200", 12, 100)
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
	s.LogRequest("key1", "127.0.0.1", "llama3", "node1", "200", 12, 100)
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
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
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
// unload schedule with no models selected is rejected — such a schedule would
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

func TestCloudBudgetExceeded_DisabledByDefault(t *testing.T) {
	s := newTestServer()
	s.TrackCloudCostModel("gpt-4o", 1000.0, 1000) // huge cost, caps still 0/disabled

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
	s.TrackCloudCostModel("gpt-4o", 2.0, 1000)

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

	s.TrackCloudCostModel("gpt-4o", 1.0, 1000) // $1.00 spent, over the $0.50 monthly cap

	exceeded, reason := s.CloudBudgetExceeded("")
	if !exceeded {
		t.Fatal("CloudBudgetExceeded = false after spend reached the monthly cap")
	}
	if reason == "" {
		t.Error("expected a non-empty reason when the monthly cap is exceeded")
	}
}

func TestHandlePredictiveDecisions_ReturnsRecordedDecisions(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	r.RecordTransition("model-a", time.Now())
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/predictive/decisions", nil)
	req.Header.Set("Authorization", "Bearer "+s.AdminToken())
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

	s.TrackCloudCostModel("gpt-4o", 1.0, 1000) // $0.001, well under the $100 cap

	if exceeded, reason := s.CloudBudgetExceeded(""); exceeded {
		t.Errorf("CloudBudgetExceeded = true (%q), want false when spend is under the cap", reason)
	}
}
