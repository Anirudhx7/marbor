package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// auditPagingServer returns a Server over a real store holding n audit rows
// (ids 1..n) written directly, so ordering is deterministic.
func auditPagingServer(t *testing.T, n int) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		if err := st.AppendAuditLog(store.AuditEntry{
			Time:      base.Add(time.Duration(i) * time.Second),
			RequestID: fmt.Sprintf("req-%d", i),
			KeyName:   "key1",
			Model:     "llama3",
			Node:      "node-a",
			Status:    "200",
		}); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	s := NewServer(r, nil, config.Config{}, st)
	s.SetAuditLogger(audit.New(st, true))
	return s
}

func getAudit(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/audit"+query, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

type auditPageResp struct {
	Entries      []map[string]any `json:"entries"`
	Total        int              `json:"total"`
	Truncated    bool             `json:"truncated"`
	NextBeforeID *int64           `json:"next_before_id"`
	PrevAfterID  *int64           `json:"prev_after_id"`
}

func decodeAuditPage(t *testing.T, rec *httptest.ResponseRecorder) auditPageResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp auditPageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func entryIDs(resp auditPageResp) []int64 {
	out := make([]int64, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, int64(e["id"].(float64)))
	}
	return out
}

func TestHandleAuditCursorBadRequests(t *testing.T) {
	s := auditPagingServer(t, 3)
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"non-numeric before", "?before_id=abc", "invalid before_id"},
		{"zero before", "?before_id=0", "invalid before_id"},
		{"negative before", "?before_id=-5", "invalid before_id"},
		{"overflow before", "?before_id=99999999999999999999", "invalid before_id"},
		{"non-numeric after", "?after_id=abc", "invalid after_id"},
		{"zero after", "?after_id=0", "invalid after_id"},
		{"negative after", "?after_id=-5", "invalid after_id"},
		{"overflow after", "?after_id=99999999999999999999", "invalid after_id"},
		{"both cursors", "?before_id=5&after_id=2", "before_id and after_id are mutually exclusive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := getAudit(t, s, c.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			if body := rec.Body.String(); !strings.Contains(body, c.want) {
				t.Fatalf("body %q does not mention %q", body, c.want)
			}
		})
	}
}

func TestHandleAuditNoCursorKeepsOldShape(t *testing.T) {
	s := auditPagingServer(t, 3)
	resp := decodeAuditPage(t, getAudit(t, s, ""))
	if resp.Total != 3 || resp.Truncated {
		t.Fatalf("total=%d truncated=%v, want 3 false", resp.Total, resp.Truncated)
	}
	if fmt.Sprint(entryIDs(resp)) != "[3 2 1]" {
		t.Fatalf("ids = %v, want [3 2 1]", entryIDs(resp))
	}
	if resp.NextBeforeID != nil || resp.PrevAfterID != nil {
		t.Fatalf("cursors present on a short first page: %+v", resp)
	}
	if resp.Entries[0]["request_id"] != "req-3" {
		t.Fatalf("request_id = %v", resp.Entries[0]["request_id"])
	}
}

func TestHandleAuditCursorPresenceFirstMiddleLast(t *testing.T) {
	s := auditPagingServer(t, 5)

	first := decodeAuditPage(t, getAudit(t, s, "?limit=2"))
	if fmt.Sprint(entryIDs(first)) != "[5 4]" || !first.Truncated || first.Total != 2 {
		t.Fatalf("first page = %+v", first)
	}
	if first.NextBeforeID == nil || *first.NextBeforeID != 4 {
		t.Fatalf("first next_before_id = %v, want 4", first.NextBeforeID)
	}
	if first.PrevAfterID != nil {
		t.Fatalf("first prev_after_id = %v, want absent", *first.PrevAfterID)
	}

	mid := decodeAuditPage(t, getAudit(t, s, "?limit=2&before_id=4"))
	if fmt.Sprint(entryIDs(mid)) != "[3 2]" {
		t.Fatalf("middle ids = %v", entryIDs(mid))
	}
	if mid.NextBeforeID == nil || *mid.NextBeforeID != 2 || mid.PrevAfterID == nil || *mid.PrevAfterID != 3 {
		t.Fatalf("middle cursors = %v / %v, want 2 / 3", mid.NextBeforeID, mid.PrevAfterID)
	}

	last := decodeAuditPage(t, getAudit(t, s, "?limit=2&before_id=2"))
	if fmt.Sprint(entryIDs(last)) != "[1]" || last.Truncated {
		t.Fatalf("last page = %+v", last)
	}
	if last.NextBeforeID != nil {
		t.Fatalf("last next_before_id = %v, want absent", *last.NextBeforeID)
	}
	if last.PrevAfterID == nil || *last.PrevAfterID != 1 {
		t.Fatalf("last prev_after_id = %v, want 1", last.PrevAfterID)
	}

	// Walking back with after_id mirrors the same pages, newest-first.
	back := decodeAuditPage(t, getAudit(t, s, "?limit=2&after_id=1"))
	if fmt.Sprint(entryIDs(back)) != "[3 2]" {
		t.Fatalf("after_id ids = %v, want [3 2]", entryIDs(back))
	}
	if back.PrevAfterID == nil || *back.PrevAfterID != 3 || back.NextBeforeID == nil || *back.NextBeforeID != 2 {
		t.Fatalf("after_id cursors = %v / %v", back.PrevAfterID, back.NextBeforeID)
	}
}

func TestHandleAuditStaleCursorEmptyNoCursors(t *testing.T) {
	s := auditPagingServer(t, 3)
	resp := decodeAuditPage(t, getAudit(t, s, "?after_id=9223372036854775807"))
	if len(resp.Entries) != 0 || resp.NextBeforeID != nil || resp.PrevAfterID != nil {
		t.Fatalf("stale cursor page = %+v, want empty and no cursors", resp)
	}
	var raw map[string]json.RawMessage
	rec := getAudit(t, s, "?after_id=9223372036854775807")
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["entries"]) != "[]" {
		t.Fatalf("entries = %s, want []", raw["entries"])
	}

	big := decodeAuditPage(t, getAudit(t, s, "?before_id=9223372036854775807"))
	if len(big.Entries) != 3 {
		t.Fatalf("MaxInt64 before_id returned %d rows, want 3", len(big.Entries))
	}
}

func TestHandleAuditLimitClampWithCursorOver1000Rows(t *testing.T) {
	s := auditPagingServer(t, 1002)
	resp := decodeAuditPage(t, getAudit(t, s, "?limit=5000"))
	if resp.Total != 1000 || !resp.Truncated {
		t.Fatalf("total=%d truncated=%v, want 1000 true", resp.Total, resp.Truncated)
	}
	if resp.NextBeforeID == nil || *resp.NextBeforeID != 3 {
		t.Fatalf("next_before_id = %v, want 3", resp.NextBeforeID)
	}
	next := decodeAuditPage(t, getAudit(t, s, fmt.Sprintf("?limit=1000&before_id=%d", *resp.NextBeforeID)))
	if fmt.Sprint(entryIDs(next)) != "[2 1]" || next.NextBeforeID != nil {
		t.Fatalf("tail page = %+v", next)
	}
}

func TestHandleAuditCursorWithSinceUntil(t *testing.T) {
	s := auditPagingServer(t, 20) // ts = 2026-01-01T00:00:<id>
	resp := decodeAuditPage(t, getAudit(t, s,
		"?since=2026-01-01T00:00:05Z&until=2026-01-01T00:00:15Z&before_id=12&limit=3"))
	if fmt.Sprint(entryIDs(resp)) != "[11 10 9]" {
		t.Fatalf("ids = %v, want [11 10 9]", entryIDs(resp))
	}
	if resp.NextBeforeID == nil || *resp.NextBeforeID != 9 {
		t.Fatalf("next_before_id = %v, want 9", resp.NextBeforeID)
	}
	if resp.PrevAfterID == nil || *resp.PrevAfterID != 11 {
		t.Fatalf("prev_after_id = %v, want 11 (id 13 and 14 are inside the window)", resp.PrevAfterID)
	}
}

func TestHandleAuditUnavailableWithoutLogger(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(r, nil, config.Config{}, st)
	rec := getAudit(t, s, "?before_id=5")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
