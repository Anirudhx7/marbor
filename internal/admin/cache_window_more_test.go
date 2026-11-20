package admin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/store"
	_ "modernc.org/sqlite"
)

// errWindowStore fails the cache-window read; everything else is the real store.
type errWindowStore struct{ store.Store }

func (errWindowStore) CacheWindowRows(int) ([]store.CacheWindowRow, error) {
	return nil, errors.New("disk on fire")
}

func bulkInsert(t *testing.T, st store.Store, n int, fn func(i int) (id, key, model string, ts int64, cached, input any)) {
	t.Helper()
	raw, err := sql.Open("sqlite", st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id, key, model, ts, cached, input := fn(i)
		if _, err := tx.Exec(`INSERT INTO request_log (id, key_name, model, node_name, status_code, ts, cached_tokens, input_tokens) VALUES (?, ?, ?, 'n', 200, ?, ?, ?)`,
			id, key, model, ts, cached, input); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCacheWindowStoreErrorIsUnavailableNotEmpty(t *testing.T) {
	srv, st := newCacheTestServer(t)
	srv.st = errWindowStore{st}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"analytics": srv.handleAnalytics,
		"export":    srv.handleAnalyticsExport,
	} {
		w := httptest.NewRecorder()
		call(w, httptest.NewRequest(http.MethodGet, "/x?type=models&format=json", nil))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s decode: %v %s", name, err, w.Body.String())
		}
		if v, present := out["cache_window"]; !present || v != nil {
			t.Errorf("%s cache_window = %v present=%v, want explicit null", name, v, present)
		}
		if out["cache_window_error"] == nil || out["cache_window_error"] == "" {
			t.Errorf("%s cache_window_error missing: %s", name, w.Body.String())
		}
	}
}

func TestCacheWindowSuccessHasNoErrorField(t *testing.T) {
	srv, _ := newCacheTestServer(t)
	w := httptest.NewRecorder()
	srv.handleAnalytics(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["cache_window_error"]; ok {
		t.Errorf("cache_window_error present on success: %v", out["cache_window_error"])
	}
}

func TestCacheWindowCapsAtThousandNewestRows(t *testing.T) {
	srv, st := newCacheTestServer(t)
	base := time.Now().Add(-3 * time.Hour).Unix()
	bulkInsert(t, st, 1100, func(i int) (string, string, string, int64, any, any) {
		model := "newer"
		if i < 100 {
			model = "oldest"
		}
		return fmt.Sprintf("r%04d", i), "k", model, base + int64(i), 1, 2
	})
	cw := getCacheWindow(t, srv)
	if cw["window_requests"] != float64(1000) {
		t.Fatalf("window_requests = %v, want 1000", cw["window_requests"])
	}
	arr := cw["by_model"].([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["model"] != "newer" {
		t.Errorf("by_model = %v, want only the 1000 newest rows (the oldest 100 dropped)", arr)
	}
	oldest, _ := time.Parse(time.RFC3339, cw["window_oldest"].(string))
	if oldest.Unix() != base+100 {
		t.Errorf("window_oldest = %d, want %d", oldest.Unix(), base+100)
	}
}

func TestCacheWindowSortAndTotals(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "1", "k2", "b", 9, i64(1), i64(10))
	seedRow(t, st, "2", "k1", "a", 8, i64(2), i64(10))
	seedRow(t, st, "3", "k1", "c", 7, i64(3), i64(10))
	seedRow(t, st, "4", "k1", "c", 6, nil, nil)
	cw := getCacheWindow(t, srv)
	var models, keys []string
	for _, m := range cw["by_model"].([]any) {
		models = append(models, m.(map[string]any)["model"].(string))
	}
	for _, k := range cw["by_key"].([]any) {
		keys = append(keys, k.(map[string]any)["key"].(string))
	}
	if fmt.Sprint(models) != "[c a b]" {
		t.Errorf("models = %v, want most requests first then name: [c a b]", models)
	}
	if fmt.Sprint(keys) != "[k1 k2]" {
		t.Errorf("keys = %v, want [k1 k2]", keys)
	}
	if cw["cached_tokens"] != float64(6) || cw["input_tokens"] != float64(30) || cw["reporting_requests"] != float64(3) {
		t.Errorf("window totals = %v/%v over %v reporting, want 6/30 over 3", cw["cached_tokens"], cw["input_tokens"], cw["reporting_requests"])
	}
}

func TestCacheWindowCountsCloudAndErrorRows(t *testing.T) {
	srv, st := newCacheTestServer(t)
	for i, c := range []struct {
		status int
		cloud  bool
	}{{200, true}, {503, false}, {500, true}} {
		err := st.AppendRequest(store.RequestRecord{ID: fmt.Sprintf("c%d", i), KeyName: "k", Model: "m", NodeName: "n", StatusCode: c.status, IsCloud: c.cloud, TS: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}
	seedRow(t, st, "ok", "k", "m", 1, i64(1), i64(2))
	cw := getCacheWindow(t, srv)
	if cw["window_requests"] != float64(4) || cw["reporting_requests"] != float64(1) {
		t.Errorf("counts = %v/%v, want 4/1 (cloud and error rows are window requests that did not report)", cw["window_requests"], cw["reporting_requests"])
	}
}

func TestCacheWindowUnknownModelAndKeyBuckets(t *testing.T) {
	srv, st := newCacheTestServer(t)
	now := time.Now().Unix()
	bulkInsert(t, st, 5, func(i int) (string, string, string, int64, any, any) {
		switch i {
		case 0:
			return "a", "", "", now, nil, nil // no key, empty model
		case 1:
			return "b", "-", "-", now, 1, 2 // a key and a model literally named "-"
		case 2:
			return "c", "", "m", now, nil, nil
		case 3:
			return "d", "", "", now, nil, nil // becomes NULL model and NULL key below
		default:
			return "e", "", "", now, nil, nil // stays an empty-string model
		}
	})
	raw, err := sql.Open("sqlite", st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE request_log SET model=NULL, key_name=NULL WHERE id IN ('a','d')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	cw := getCacheWindow(t, srv)
	count := func(list, nameField, flag, name string, flagged bool) float64 {
		var total float64
		for _, g := range cw[list].([]any) {
			gm := g.(map[string]any)
			if gm[nameField] != name || (gm[flag] == true) != flagged {
				continue
			}
			total += gm["window_requests"].(float64)
		}
		return total
	}
	if got := count("by_model", "model", "no_model", "-", true); got != 3 {
		t.Errorf("unknown-model bucket = %v requests, want 3 (empty and NULL model)", got)
	}
	if got := count("by_model", "model", "no_model", "-", false); got != 1 {
		t.Errorf("model named - bucket = %v requests, want its own bucket of 1", got)
	}
	if got := count("by_key", "key", "no_key", "-", true); got != 4 {
		t.Errorf("no-key bucket = %v requests, want 4 (empty and NULL key_name)", got)
	}
	if got := count("by_key", "key", "no_key", "-", false); got != 1 {
		t.Errorf("key named - bucket = %v requests, want its own bucket of 1", got)
	}
}

func TestCacheStatsAddCountsOnlyValidSplits(t *testing.T) {
	var c CacheStats
	c.add(i64(store.MaxCacheSplitTokens), i64(store.MaxCacheSplitTokens))
	c.add(i64(store.MaxCacheSplitTokens+1), i64(1)) // over cap
	c.add(nil, nil)
	c.add(i64(5), nil)    // half set
	c.add(i64(5), i64(6)) // cached above input
	c.add(i64(0), i64(0))
	if c.WindowRequests != 6 || c.ReportingRequests != 1 {
		t.Errorf("counts = %d/%d, want 6 window and 1 reporting", c.WindowRequests, c.ReportingRequests)
	}
	if c.InputTokens != store.MaxCacheSplitTokens || c.CachedTokens != store.MaxCacheSplitTokens {
		t.Errorf("sums = %d/%d, want only the valid row", c.CachedTokens, c.InputTokens)
	}
	c.finish()
	if c.HitPct == nil || *c.HitPct != 100 {
		t.Errorf("HitPct = %v, want 100", c.HitPct)
	}
}

func TestCacheWindowTotalCarriesHitPct(t *testing.T) {
	srv, st := newCacheTestServer(t)
	seedRow(t, st, "1", "k", "a", 2, i64(1), i64(4))
	seedRow(t, st, "2", "k", "b", 1, i64(3), i64(4))
	cw := getCacheWindow(t, srv)
	if cw["hit_pct"] != float64(50) {
		t.Errorf("window hit_pct = %v, want 50 (4 of 8)", cw["hit_pct"])
	}
}

func TestLogRequestValidatesAndCopiesUsage(t *testing.T) {
	srv, st := newCacheTestServer(t)
	cached, input := i64(7), i64(9)
	srv.LogRequest(RequestLogParams{RequestID: "good", Model: "m", Usage: RequestUsage{Input: input, Cached: cached}})
	srv.LogRequest(RequestLogParams{RequestID: "over", Model: "m", Usage: RequestUsage{Cached: i64(10), Input: i64(9)}})
	srv.LogRequest(RequestLogParams{RequestID: "half", Model: "m", Usage: RequestUsage{Cached: i64(1)}})
	srv.LogRequest(RequestLogParams{RequestID: "zeroin", Model: "m", Usage: RequestUsage{Cached: i64(0), Input: i64(0)}})
	srv.LogRequest(RequestLogParams{RequestID: "neg", Model: "m", Usage: RequestUsage{Cached: i64(-1), Input: i64(5)}})
	*cached = 999 // the caller's pointer must not be what the ring or the store holds

	srv.mu.RLock()
	for _, r := range srv.requests {
		switch r.ID {
		case "good":
			if r.CachedTokens == nil || *r.CachedTokens != 7 || *r.InputTokens != 9 {
				t.Errorf("ring good = %v/%v, want 7/9 (a copy)", r.CachedTokens, r.InputTokens)
			}
		default:
			if r.CachedTokens != nil || r.InputTokens != nil {
				t.Errorf("ring %s = %v/%v, want nil for an invalid split", r.ID, r.CachedTokens, r.InputTokens)
			}
		}
	}
	srv.mu.RUnlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs, _ := st.LastRequests(10)
		if len(recs) == 5 {
			for _, r := range recs {
				if r.ID == "good" && (r.CachedTokens == nil || *r.CachedTokens != 7) {
					t.Errorf("persisted good = %v, want 7", r.CachedTokens)
				}
				if r.ID != "good" && (r.CachedTokens != nil || r.InputTokens != nil) {
					t.Errorf("persisted %s = %v/%v, want nil", r.ID, r.CachedTokens, r.InputTokens)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted %d rows, want 5", len(recs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- /admin/audit join ---

func auditJoinServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, st := newCacheTestServer(t)
	srv.SetAuditLogger(audit.New(st, true))
	return srv, st
}

func appendAudit(t *testing.T, st store.Store, id string) {
	t.Helper()
	if err := st.AppendAuditLog(store.AuditEntry{Time: time.Now(), RequestID: id, KeyName: "k", Model: "m", Node: "n", Status: "200"}); err != nil {
		t.Fatal(err)
	}
}

func TestHandleAuditCarriesCacheSplitFromRequestLog(t *testing.T) {
	srv, st := auditJoinServer(t)
	seedRow(t, st, "val", "k", "m", 5, i64(1820), i64(2000))
	seedRow(t, st, "zero", "k", "m", 5, i64(0), i64(1450))
	seedRow(t, st, "null", "k", "m", 5, nil, nil)
	for _, id := range []string{"val", "zero", "null", "trimmed"} {
		appendAudit(t, st, id)
	}
	// A row that violates the invariant (written by something else).
	seedRow(t, st, "bad", "k", "m", 5, i64(1), i64(2))
	raw, err := sql.Open("sqlite", st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE request_log SET cached_tokens=50 WHERE id='bad'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	appendAudit(t, st, "bad")

	resp := decodeAuditPage(t, getAudit(t, srv, ""))
	byID := map[string]map[string]any{}
	for _, e := range resp.Entries {
		byID[e["request_id"].(string)] = e
	}
	if e := byID["val"]; e["cachedTokens"] != float64(1820) || e["inputTokens"] != float64(2000) {
		t.Errorf("joined row = %v, want 1820/2000", e)
	}
	if e := byID["zero"]; e["cachedTokens"] != float64(0) || e["inputTokens"] != float64(1450) {
		t.Errorf("real-zero row = %v, want a kept 0 over 1450", e)
	}
	for _, id := range []string{"null", "trimmed", "bad"} {
		if _, ok := byID[id]["cachedTokens"]; ok {
			t.Errorf("%s row has cachedTokens, want omitted", id)
		}
		if _, ok := byID[id]["inputTokens"]; ok {
			t.Errorf("%s row has inputTokens, want omitted", id)
		}
	}
	// The persisted fields are unchanged.
	if byID["val"]["key_name"] != "k" || byID["val"]["status"] != "200" || byID["val"]["id"] == nil {
		t.Errorf("existing audit fields changed: %v", byID["val"])
	}
}

func TestHandleAuditStoreErrorLeavesEntriesUndecorated(t *testing.T) {
	srv, st := auditJoinServer(t)
	seedRow(t, st, "x", "k", "m", 5, i64(1820), i64(2000))
	appendAudit(t, st, "x")
	srv.st = errSplitStore{st}
	w := getAudit(t, srv, "")
	resp := decodeAuditPage(t, w)
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	if _, ok := resp.Entries[0]["cachedTokens"]; ok {
		t.Errorf("entry carries cachedTokens although the lookup failed: %v", resp.Entries[0])
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["cache_split_error"] != auditCacheSplitUnavailable {
		t.Errorf("cache_split_error = %v, want %q", out["cache_split_error"], auditCacheSplitUnavailable)
	}
}

func TestHandleAuditSplitLookupFailureLogsOnStateChange(t *testing.T) {
	srv, st := auditJoinServer(t)
	appendAudit(t, st, "x")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	srv.st = errSplitStore{st}
	for i := 0; i < 3; i++ {
		getAudit(t, srv, "")
	}
	if n := strings.Count(buf.String(), "failing"); n != 1 {
		t.Errorf("failure logged %d times over 3 polls, want 1: %q", n, buf.String())
	}
	srv.st = st
	w := getAudit(t, srv, "")
	if strings.Contains(w.Body.String(), "cache_split_error") {
		t.Errorf("error marker present after recovery: %s", w.Body.String())
	}
	if !strings.Contains(buf.String(), "recovered") {
		t.Errorf("recovery not logged: %q", buf.String())
	}
}

type errSplitStore struct{ store.Store }

func (errSplitStore) RequestCacheSplits([]string) (map[string]store.CacheSplit, error) {
	return nil, errors.New("boom")
}

func TestHandleAuditEmptyPageStillAnArray(t *testing.T) {
	srv, _ := auditJoinServer(t)
	w := getAudit(t, srv, "")
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if string(out["entries"]) != "[]" {
		t.Errorf("entries = %s, want []", out["entries"])
	}
}
