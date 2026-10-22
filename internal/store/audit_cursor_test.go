package store

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openCursorStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "cursor.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedAudit appends n rows named req-1..req-n (ids 1..n, timestamps one
// second apart from 2026-01-01) with alternating model and status so filters
// have something to bite on. Odd rows: llama3 / 200. Even rows: qwen_x% / 500.
func seedAudit(t *testing.T, st Store, n int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		model := "llama3"
		status := "200"
		if i%2 == 0 {
			model = "qwen_x%"
			status = "500"
		}
		e := AuditEntry{
			Time:      base.Add(time.Duration(i) * time.Second),
			RequestID: fmt.Sprintf("req-%d", i),
			KeyName:   "key-a",
			Model:     model,
			Node:      "node-1",
			Status:    status,
		}
		if err := st.AppendAuditLog(e); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}
}

func auditIDs(es []AuditEntry) string {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return fmt.Sprint(out)
}

func assertDescending(t *testing.T, es []AuditEntry) {
	t.Helper()
	for i := 1; i < len(es); i++ {
		if es[i-1].ID <= es[i].ID {
			t.Fatalf("not strictly newest-first: %s", auditIDs(es))
		}
	}
}

func TestQueryAuditLogCarriesID(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 3)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[3 2 1]"; g != w {
		t.Fatalf("ids = %s, want %s", g, w)
	}
}

func TestQueryAuditLogBeforeIDWalkNoGapNoDup(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 250)

	seen := map[int64]bool{}
	var before int64
	pages := 0
	for {
		got, err := st.QueryAuditLog(AuditQuery{Limit: 100, BeforeID: before})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		assertDescending(t, got)
		for _, e := range got {
			if seen[e.ID] {
				t.Fatalf("duplicate id %d", e.ID)
			}
			seen[e.ID] = true
		}
		before = got[len(got)-1].ID
		pages++
		// Rows arriving between pages must not shift the older pages.
		if pages == 1 {
			seedAudit(t, st, 5)
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	// The first page was the newest 100 of 250 (ids 151..250); the 5 rows
	// inserted afterwards (251..255) are newer than the walk and never appear
	// in it, so the walk covers 1..250 minus nothing.
	if len(seen) != 250 {
		t.Fatalf("saw %d rows, want 250", len(seen))
	}
	for i := int64(1); i <= 250; i++ {
		if !seen[i] {
			t.Fatalf("missing id %d", i)
		}
	}
}

func TestQueryAuditLogAfterIDNewestFirstRoundTrip(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 10)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 3, AfterID: 4})
	if err != nil {
		t.Fatal(err)
	}
	// The three rows just above the cursor, still newest-first.
	if g, w := auditIDs(got), "[7 6 5]"; g != w {
		t.Fatalf("ids = %s, want %s", g, w)
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 3, AfterID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[10 9 8]"; g != w {
		t.Fatalf("ids = %s, want %s", g, w)
	}
}

func TestQueryAuditLogCursorWithFilters(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 20)

	// A literal "%" must match only itself, not act as a wildcard.
	got, err := st.QueryAuditLog(AuditQuery{Limit: 100, Model: "x%", BeforeID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[10 8 6 4 2]"; g != w {
		t.Fatalf("model+before ids = %s, want %s", g, w)
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 100, StatusCategory: "success", AfterID: 15})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[19 17]"; g != w {
		t.Fatalf("status+after ids = %s, want %s", g, w)
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 100, Key: "key-a", Node: "node-1", BeforeID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[3 2 1]"; g != w {
		t.Fatalf("key+node ids = %s, want %s", g, w)
	}
	// "_" is literal: matches the stored underscore, not any character.
	got, err = st.QueryAuditLog(AuditQuery{Limit: 100, Model: "qwen_x", BeforeID: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("literal underscore matched %d rows, want 10", len(got))
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 100, Model: "qwenXx", BeforeID: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("underscore acted as a wildcard: %d rows", len(got))
	}
}

func TestQueryAuditLogCursorWithSinceUntil(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 20)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	since := base.Add(5 * time.Second)  // ts > 5s => id >= 6
	until := base.Add(15 * time.Second) // ts < 15s => id <= 14

	got, err := st.QueryAuditLog(AuditQuery{Limit: 100, Since: since, Until: until, BeforeID: 12})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[11 10 9 8 7 6]"; g != w {
		t.Fatalf("before ids = %s, want %s", g, w)
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 100, Since: since, Until: until, AfterID: 12})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[14 13]"; g != w {
		t.Fatalf("after ids = %s, want %s", g, w)
	}
}

func TestQueryAuditLogEmptyAndStaleCursors(t *testing.T) {
	st := openCursorStore(t)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 10})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty table: got %d rows, err %v", len(got), err)
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 10, BeforeID: 5})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty table before: got %d rows, err %v", len(got), err)
	}

	seedAudit(t, st, 5)
	cases := []struct {
		name string
		q    AuditQuery
		want int
	}{
		{"before MaxInt64", AuditQuery{Limit: 10, BeforeID: math.MaxInt64}, 5},
		{"after MaxInt64", AuditQuery{Limit: 10, AfterID: math.MaxInt64}, 0},
		{"after max id", AuditQuery{Limit: 10, AfterID: 5}, 0},
		{"before 1", AuditQuery{Limit: 10, BeforeID: 1}, 0},
	}
	for _, c := range cases {
		got, err := st.QueryAuditLog(c.q)
		if err != nil || len(got) != c.want {
			t.Errorf("%s: got %d rows, err %v, want %d", c.name, len(got), err, c.want)
		}
	}
}

func TestQueryAuditLogLimit1001Rows(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 1005)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1001 {
		t.Fatalf("got %d rows, want 1001", len(got))
	}
	got, err = st.QueryAuditLog(AuditQuery{Limit: 1001, BeforeID: got[len(got)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("tail got %d rows, want 4", len(got))
	}
}

func TestQueryAuditLogZeroCursorsMatchOldBehaviour(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 30)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 30 {
		t.Fatalf("got %d rows, want 30", len(got))
	}
	assertDescending(t, got)
	got, err = st.QueryAuditLog(AuditQuery{}) // default limit
	if err != nil || len(got) != 30 {
		t.Fatalf("default limit: %d rows, err %v", len(got), err)
	}
}

func TestBuildAuditQueryCursorUsesPrimaryKey(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 500)
	s := st.(*sqliteStore)

	for name, opts := range map[string]AuditQuery{
		"before": {Limit: 11, BeforeID: 40},
		"after":  {Limit: 11, AfterID: 10},
	} {
		q, args := buildAuditQuery(opts)
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var details []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("%s: scan: %v", name, err)
			}
			details = append(details, detail)
		}
		rows.Close()
		joined := strings.Join(details, " | ")
		if !strings.Contains(joined, "SEARCH") {
			t.Errorf("%s: plan lacks SEARCH: %s", name, joined)
		}
		if !strings.Contains(joined, "USING INTEGER PRIMARY KEY") {
			t.Errorf("%s: plan does not use the integer primary key: %s", name, joined)
		}
		for _, d := range details {
			if strings.HasPrefix(d, "SCAN") {
				t.Errorf("%s: bare table scan: %s", name, d)
			}
		}
	}
}

func TestQueryAuditLogRejectsInvalidCursors(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 5)
	for name, opts := range map[string]AuditQuery{
		"both":            {BeforeID: 4, AfterID: 2},
		"negative before": {BeforeID: -1},
		"negative after":  {AfterID: -1},
	} {
		if _, err := st.QueryAuditLog(opts); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
	_, err := st.QueryAuditLog(AuditQuery{BeforeID: 4, AfterID: 2})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both cursors error = %v, want mutually exclusive", err)
	}
}

// Since is a strict lower bound and Until a strict upper bound: a row stamped
// exactly on either bound is excluded.
func TestQueryAuditLogSinceUntilAreStrictBounds(t *testing.T) {
	st := openCursorStore(t)
	seedAudit(t, st, 10)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := st.QueryAuditLog(AuditQuery{Limit: 100, Since: base.Add(3 * time.Second), Until: base.Add(7 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if g, w := auditIDs(got), "[6 5 4]"; g != w {
		t.Fatalf("ids = %s, want %s (rows at the bounds excluded)", g, w)
	}
}
