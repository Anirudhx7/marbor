package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
	_ "modernc.org/sqlite"
)

// Rows in this file are synthetic: written by the tests, not captured from an
// engine. Stores are file-backed because every in-memory store in a process
// shares one database, which would leak rows between tests.

func i64(n int64) *int64 { return &n }

func newCacheTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(config.RoutingConfig{}, nil, nil)
	srv := NewServer(r, nil, config.Config{}, st)
	t.Cleanup(func() { st.Close() })
	t.Cleanup(srv.Shutdown)
	return srv, st
}

func seedRow(t *testing.T, st store.Store, id, key, model string, age int, cached, input *int64) {
	t.Helper()
	err := st.AppendRequest(store.RequestRecord{
		ID: id, KeyName: key, Model: model, NodeName: "n1", StatusCode: 200,
		TS:           time.Now().Add(-time.Duration(age) * time.Second),
		CachedTokens: cached, InputTokens: input,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func getCacheWindow(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleAnalytics(w, httptest.NewRequest(http.MethodGet, "/admin/analytics", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body %s", err, w.Body.String())
	}
	cw, ok := out["cache_window"].(map[string]any)
	if !ok {
		t.Fatalf("cache_window missing or wrong type: %v", out["cache_window"])
	}
	return cw
}

func findGroup(t *testing.T, cw map[string]any, list, field, name string) map[string]any {
	t.Helper()
	arr, _ := cw[list].([]any)
	for _, e := range arr {
		m := e.(map[string]any)
		if m[field] == name {
			return m
		}
	}
	t.Fatalf("%s has no %s=%q: %v", list, field, name, arr)
	return nil
}

func TestCacheWindowEmptyWindowHasNullHitPct(t *testing.T) {
	srv, _ := newCacheTestServer(t)
	cw := getCacheWindow(t, srv)
	if cw["window_requests"] != float64(0) || cw["reporting_requests"] != float64(0) {
		t.Errorf("empty window counts = %v/%v, want 0/0", cw["window_requests"], cw["reporting_requests"])
	}
	if cw["window_oldest"] != nil {
		t.Errorf("window_oldest = %v, want null", cw["window_oldest"])
	}
	if arr, ok := cw["by_model"].([]any); !ok || len(arr) != 0 {
		t.Errorf("by_model = %v, want empty array", cw["by_model"])
	}
	if arr, ok := cw["by_key"].([]any); !ok || len(arr) != 0 {
		t.Errorf("by_key = %v, want empty array", cw["by_key"])
	}
}

func TestCacheWindowDenominatorExcludesNonReporting(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k1", "m1", 5, i64(80), i64(100))
	seedRow(t, st, "b", "k1", "m1", 4, i64(0), i64(100))
	seedRow(t, st, "c", "k1", "m1", 3, nil, nil) // not reported
	cw := getCacheWindow(t, srv)
	if cw["window_requests"] != float64(3) || cw["reporting_requests"] != float64(2) {
		t.Fatalf("counts = %v/%v, want 3/2", cw["window_requests"], cw["reporting_requests"])
	}
	m := findGroup(t, cw, "by_model", "model", "m1")
	if m["cached_tokens"] != float64(80) || m["input_tokens"] != float64(200) || m["hit_pct"] != float64(40) {
		t.Errorf("m1 = %v, want cached 80 input 200 hit 40 (the unreported row must not enter the denominator)", m)
	}
	if m["window_requests"] != float64(3) || m["reporting_requests"] != float64(2) {
		t.Errorf("m1 counts = %v", m)
	}
}

func TestCacheWindowRealZeroIsNotNull(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k1", "m1", 5, i64(0), i64(1450))
	m := findGroup(t, getCacheWindow(t, srv), "by_model", "model", "m1")
	if m["hit_pct"] != float64(0) {
		t.Errorf("hit_pct = %v, want 0 for a reported zero", m["hit_pct"])
	}
}

func TestCacheWindowNonReportingModelHasNullHitPct(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k1", "ollama-model", 5, nil, nil)
	m := findGroup(t, getCacheWindow(t, srv), "by_model", "model", "ollama-model")
	if m["hit_pct"] != nil || m["reporting_requests"] != float64(0) {
		t.Errorf("group = %v, want hit_pct null and reporting 0", m)
	}
}

func TestCacheWindowAliasArrowGroupsByResolvedModel(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k1", "alias -> requested -> resolved", 5, i64(10), i64(20))
	seedRow(t, st, "b", "k1", "requested -> resolved", 4, i64(30), i64(40))
	seedRow(t, st, "c", "k1", "resolved", 3, i64(0), i64(40))
	cw := getCacheWindow(t, srv)
	arr := cw["by_model"].([]any)
	if len(arr) != 1 {
		t.Fatalf("by_model = %v, want one group named resolved", arr)
	}
	m := findGroup(t, cw, "by_model", "model", "resolved")
	if m["window_requests"] != float64(3) || m["cached_tokens"] != float64(40) || m["input_tokens"] != float64(100) {
		t.Errorf("resolved = %v", m)
	}
}

func TestCacheWindowByKeyIncludingEmptyKeyAndErrorRows(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "team", "m1", 5, i64(50), i64(100))
	seedRow(t, st, "b", "", "m1", 4, nil, nil)
	cw := getCacheWindow(t, srv)
	team := findGroup(t, cw, "by_key", "key", "team")
	if team["hit_pct"] != float64(50) {
		t.Errorf("team = %v", team)
	}
	anon := findGroup(t, cw, "by_key", "key", "-")
	if anon["window_requests"] != float64(1) || anon["hit_pct"] != nil {
		t.Errorf("empty key group = %v, want key - with 1 request and null hit", anon)
	}
}

func TestCacheWindowViolatingRowCountsAsNonReporting(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k", "m1", 5, i64(150), i64(100)) // cached > input
	seedRow(t, st, "b", "k", "m1", 4, i64(0), i64(0))     // zero input
	seedRow(t, st, "c", "k", "m1", 3, i64(5), i64(10))
	m := findGroup(t, getCacheWindow(t, srv), "by_model", "model", "m1")
	if m["reporting_requests"] != float64(1) || m["input_tokens"] != float64(10) || m["hit_pct"] != float64(50) {
		t.Errorf("m1 = %v, want only the sane row counted", m)
	}
}

func TestCacheWindowOldestAndSize(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k", "m1", 100, i64(1), i64(2))
	seedRow(t, st, "b", "k", "m1", 1, i64(1), i64(2))
	cw := getCacheWindow(t, srv)
	if cw["window_size"] != float64(1000) {
		t.Errorf("window_size = %v", cw["window_size"])
	}
	s, _ := cw["window_oldest"].(string)
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil || time.Since(ts) < 90*time.Second || time.Since(ts) > 150*time.Second {
		t.Errorf("window_oldest = %q (%v), want about 100s ago", s, err)
	}
}

func TestCacheWindowLeavesByModelUntouched(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k", "cachewin", 1, i64(1), i64(2))
	srv.analytics.recordLocal("served-model", 40, 0)
	w := httptest.NewRecorder()
	srv.handleAnalytics(w, httptest.NewRequest(http.MethodGet, "/admin/analytics", nil))
	var out struct {
		ByModel []struct {
			Model string `json:"model"`
		} `json:"by_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.ByModel) != 1 || out.ByModel[0].Model != "served-model" {
		t.Errorf("by_model = %+v, want only the analytics model, not the cache window's", out.ByModel)
	}
}

func TestCacheWindowInJSONExport(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "a", "k", "m1", 1, i64(1), i64(2))
	w := httptest.NewRecorder()
	srv.handleAnalyticsExport(w, httptest.NewRequest(http.MethodGet, "/admin/analytics/export?type=models&format=json", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body %s", err, w.Body.String())
	}
	if _, ok := out["cache_window"].(map[string]any); !ok {
		t.Errorf("export lacks cache_window: %s", w.Body.String())
	}
}

// Rows written before the cache columns existed read back as unknown, on the
// boot restore of the request ring, the ordered list read and a single-row
// lookup, never as 0. The row is written into a real historical schema (the
// DDL a released binary ran), then migrated by the current store.Open.
func TestPreMigrationRowsReadNilOnBootRestoreAndGetRequest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	raw, err := os.ReadFile("../store/testdata/schema_history/v0.20.0.sql")
	if err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(raw), "@@STMT@@\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := rawDB.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			t.Fatalf("historical DDL: %v\n%s", err, stmt)
		}
	}
	if _, err := rawDB.Exec(`INSERT INTO request_log (id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts) VALUES ('old','k','m1','n1',200,5,10,0,'n1',0,?)`, time.Now().Unix()-5); err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := router.New(config.RoutingConfig{}, nil, nil)
	srv := NewServer(r, nil, config.Config{}, st)
	defer srv.Shutdown()
	if err := srv.LoadFromStore(); err != nil {
		t.Fatal(err)
	}

	srv.mu.RLock()
	if len(srv.requests) != 1 || srv.requests[0].CachedTokens != nil || srv.requests[0].InputTokens != nil {
		t.Errorf("restored ring = %+v, want one row with nil cache fields", srv.requests)
	}
	srv.mu.RUnlock()
	recs, err := st.LastRequests(10)
	if err != nil || len(recs) != 1 || recs[0].CachedTokens != nil || recs[0].InputTokens != nil {
		t.Errorf("LastRequests = %+v err=%v, want one row with nil cache fields", recs, err)
	}
	rec, ok, err := st.GetRequest("old")
	if err != nil || !ok || rec.CachedTokens != nil || rec.InputTokens != nil {
		t.Errorf("GetRequest = %+v ok=%v err=%v, want nil cache fields", rec, ok, err)
	}
	cw := getCacheWindow(t, srv)
	if cw["window_requests"] != float64(1) || cw["reporting_requests"] != float64(0) {
		t.Errorf("window over an old row = %v/%v, want 1/0", cw["window_requests"], cw["reporting_requests"])
	}
}

func TestBootRestoreCarriesCacheFields(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedRow(t, st, "new", "k", "m1", 1, i64(7), i64(9))
	r := router.New(config.RoutingConfig{}, nil, nil)
	srv := NewServer(r, nil, config.Config{}, st)
	defer srv.Shutdown()
	if err := srv.LoadFromStore(); err != nil {
		t.Fatal(err)
	}
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	got := srv.requests[0]
	if got.CachedTokens == nil || *got.CachedTokens != 7 || got.InputTokens == nil || *got.InputTokens != 9 {
		t.Errorf("restored row = %+v, want cached 7 input 9", got)
	}
}

func TestRequestsEndpointsCarryUsageAndOmitWhenNil(t *testing.T) {
	srv, _ := newCacheTestServer(t)
	srv.LogRequest(RequestLogParams{RequestID: "with", KeyName: "k", SourceIP: "ip", Model: "m", Node: "gpu-0", Status: "warm", HTTPStatus: 200, LatencyMs: 5, Tokens: 100, Usage: RequestUsage{Input: i64(2000), Cached: i64(1820)}})
	srv.LogRequest(RequestLogParams{RequestID: "zero", KeyName: "k", SourceIP: "ip", Model: "m", Node: "gpu-0", Status: "warm", HTTPStatus: 200, LatencyMs: 5, Tokens: 100, Usage: RequestUsage{Input: i64(1450), Cached: i64(0)}})
	srv.LogRequest(RequestLogParams{RequestID: "none", KeyName: "k", SourceIP: "ip", Model: "m", Node: "gpu-0", Status: "warm", HTTPStatus: 200, LatencyMs: 5, Tokens: 100})

	for _, h := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{{"requests", srv.handleRequests}, {"live", srv.handleLiveRequests}} {
		w := httptest.NewRecorder()
		h.fn(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		var out []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 3 {
			t.Fatalf("%s decode: %v %s", h.name, err, w.Body.String())
		}
		none, zero, with := out[0], out[1], out[2] // newest first
		if with["cachedTokens"] != float64(1820) || with["inputTokens"] != float64(2000) {
			t.Errorf("%s with = %v", h.name, with)
		}
		if v, ok := zero["cachedTokens"]; !ok || v != float64(0) {
			t.Errorf("%s zero row cachedTokens = %v present=%v, want a real 0", h.name, v, ok)
		}
		if _, ok := none["cachedTokens"]; ok {
			t.Errorf("%s none row has cachedTokens, want omitted", h.name)
		}
		if _, ok := none["inputTokens"]; ok {
			t.Errorf("%s none row has inputTokens, want omitted", h.name)
		}
	}
}
