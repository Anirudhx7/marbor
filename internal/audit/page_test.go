package audit

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// pageLogger returns a Logger over a real SQLite store seeded directly (no
// async writer) with n rows, ids 1..n. Odd rows use model "m-odd", even rows
// "m-even".
func pageLogger(t *testing.T, n int) *Logger {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "page.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		model := "m-odd"
		if i%2 == 0 {
			model = "m-even"
		}
		if err := st.AppendAuditLog(store.AuditEntry{
			Time:      base.Add(time.Duration(i) * time.Second),
			RequestID: fmt.Sprintf("r%d", i),
			KeyName:   "k",
			Model:     model,
			Node:      "n",
			Status:    "200",
		}); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}
	return New(st, true)
}

func pageIDs(p Page) string {
	out := make([]int64, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.ID)
	}
	return fmt.Sprint(out)
}

func TestQueryPageNilLogger(t *testing.T) {
	var l *Logger
	p, err := l.QueryPage(QueryOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 0 || p.NextBeforeID != 0 || p.PrevAfterID != 0 {
		t.Fatalf("nil logger page = %+v, want empty", p)
	}
}

func TestQueryPageFirstPage(t *testing.T) {
	l := pageLogger(t, 5)
	p, err := l.QueryPage(QueryOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[5 4]" {
		t.Fatalf("entries = %s, want [5 4]", g)
	}
	// Cursor value is the edge of the returned page, not the extra row.
	if p.NextBeforeID != 4 {
		t.Errorf("NextBeforeID = %d, want 4", p.NextBeforeID)
	}
	if p.PrevAfterID != 0 {
		t.Errorf("PrevAfterID = %d, want 0 on the newest page", p.PrevAfterID)
	}
}

func TestQueryPageMiddleAndLastPages(t *testing.T) {
	l := pageLogger(t, 5)
	p, err := l.QueryPage(QueryOptions{Limit: 2, BeforeID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[3 2]" {
		t.Fatalf("entries = %s, want [3 2]", g)
	}
	if p.NextBeforeID != 2 || p.PrevAfterID != 3 {
		t.Errorf("cursors = next %d prev %d, want 2 and 3", p.NextBeforeID, p.PrevAfterID)
	}

	p, err = l.QueryPage(QueryOptions{Limit: 2, BeforeID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[1]" {
		t.Fatalf("entries = %s, want [1]", g)
	}
	if p.NextBeforeID != 0 {
		t.Errorf("NextBeforeID = %d, want 0 on the last page", p.NextBeforeID)
	}
	if p.PrevAfterID != 1 {
		t.Errorf("PrevAfterID = %d, want 1", p.PrevAfterID)
	}
}

func TestQueryPageRowsEqualLimitHasNoNext(t *testing.T) {
	l := pageLogger(t, 4)
	p, err := l.QueryPage(QueryOptions{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 4 || p.NextBeforeID != 0 || p.PrevAfterID != 0 {
		t.Fatalf("page = %+v, want 4 rows and no cursors", p)
	}
}

func TestQueryPageRowsLimitPlusOneHasNext(t *testing.T) {
	l := pageLogger(t, 5)
	p, err := l.QueryPage(QueryOptions{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 4 {
		t.Fatalf("got %d entries, want 4 (extra row trimmed)", len(p.Entries))
	}
	if p.NextBeforeID != 2 {
		t.Errorf("NextBeforeID = %d, want 2", p.NextBeforeID)
	}
}

func TestQueryPageAfterIDDropsFarthestRow(t *testing.T) {
	l := pageLogger(t, 10)
	p, err := l.QueryPage(QueryOptions{Limit: 3, AfterID: 4})
	if err != nil {
		t.Fatal(err)
	}
	// Rows just above the cursor, newest-first; the farthest row (8) is the
	// extra one and is dropped, not the nearest.
	if g := pageIDs(p); g != "[7 6 5]" {
		t.Fatalf("entries = %s, want [7 6 5]", g)
	}
	if p.PrevAfterID != 7 {
		t.Errorf("PrevAfterID = %d, want 7 (newer rows exist)", p.PrevAfterID)
	}
	if p.NextBeforeID != 5 {
		t.Errorf("NextBeforeID = %d, want 5 (older rows exist)", p.NextBeforeID)
	}

	// Reaching the live end: no newer row.
	p, err = l.QueryPage(QueryOptions{Limit: 3, AfterID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[10 9 8]" {
		t.Fatalf("entries = %s, want [10 9 8]", g)
	}
	if p.PrevAfterID != 0 {
		t.Errorf("PrevAfterID = %d, want 0 at the newest end", p.PrevAfterID)
	}
	if p.NextBeforeID != 8 {
		t.Errorf("NextBeforeID = %d, want 8", p.NextBeforeID)
	}
}

func TestQueryPagePeeksHonourFilters(t *testing.T) {
	l := pageLogger(t, 10) // even ids are m-even: 10 8 6 4 2
	p, err := l.QueryPage(QueryOptions{Limit: 2, Model: "m-even", BeforeID: 8})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[6 4]" {
		t.Fatalf("entries = %s, want [6 4]", g)
	}
	// Newer matching row (8) exists even though id 7 and 9 do not match.
	if p.PrevAfterID != 6 {
		t.Errorf("PrevAfterID = %d, want 6", p.PrevAfterID)
	}
	if p.NextBeforeID != 4 {
		t.Errorf("NextBeforeID = %d, want 4", p.NextBeforeID)
	}

	// After the last matching older row: no next even though id 1 exists.
	p, err = l.QueryPage(QueryOptions{Limit: 5, Model: "m-even", BeforeID: 11})
	if err != nil {
		t.Fatal(err)
	}
	if g := pageIDs(p); g != "[10 8 6 4 2]" {
		t.Fatalf("entries = %s, want [10 8 6 4 2]", g)
	}
	if p.NextBeforeID != 0 {
		t.Errorf("NextBeforeID = %d, want 0 (id 1 does not match the filter)", p.NextBeforeID)
	}
	// A stale cursor with no newer match: BeforeID=11 has no newer row.
	if p.PrevAfterID != 0 {
		t.Errorf("PrevAfterID = %d, want 0", p.PrevAfterID)
	}
}

func TestQueryPageStaleCursorIsEmptyWithNoCursors(t *testing.T) {
	l := pageLogger(t, 3)
	p, err := l.QueryPage(QueryOptions{Limit: 5, AfterID: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 0 || p.NextBeforeID != 0 || p.PrevAfterID != 0 {
		t.Fatalf("stale after_id page = %+v, want empty and no cursors", p)
	}
	p, err = l.QueryPage(QueryOptions{Limit: 5, BeforeID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 0 || p.NextBeforeID != 0 || p.PrevAfterID != 0 {
		t.Fatalf("before_id=1 page = %+v, want empty and no cursors", p)
	}
}

func TestQueryForwardsCursors(t *testing.T) {
	l := pageLogger(t, 6)
	got, err := l.Query(QueryOptions{Limit: 10, BeforeID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != 3 {
		t.Fatalf("Query before_id=4 = %d rows first id %d, want 3 rows from id 3", len(got), got[0].ID)
	}
	got, err = l.Query(QueryOptions{Limit: 10, AfterID: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 6 {
		t.Fatalf("Query after_id=4 = %d rows", len(got))
	}
}
