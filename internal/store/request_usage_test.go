package store_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

func int64p(n int64) *int64 { return &n }

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		names = append(names, name+" "+typ)
	}
	sort.Strings(names)
	return names
}

// A reported cached count round-trips, a reported zero stays a zero, and a
// row with no split reads back nil, never 0.
func TestRequestUsage_NullVersusZeroRoundTrip(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for i, c := range []struct {
		id            string
		cached, input *int64
	}{
		{"val", int64p(1820), int64p(2000)},
		{"zero", int64p(0), int64p(1450)},
		{"none", nil, nil},
	} {
		err := s.AppendRequest(store.RequestRecord{ID: c.id, KeyName: "k", Model: "m", NodeName: "n", StatusCode: 200, TS: now.Add(time.Duration(i) * time.Second), CachedTokens: c.cached, InputTokens: c.input})
		if err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string][2]*int64{"val": {int64p(1820), int64p(2000)}, "zero": {int64p(0), int64p(1450)}, "none": {nil, nil}} {
		got, ok, err := s.GetRequest(id)
		if err != nil || !ok {
			t.Fatalf("GetRequest %s: %v ok=%v", id, err, ok)
		}
		if !reflect.DeepEqual(got.CachedTokens, want[0]) || !reflect.DeepEqual(got.InputTokens, want[1]) {
			t.Errorf("GetRequest %s = %v/%v, want %v/%v", id, got.CachedTokens, got.InputTokens, want[0], want[1])
		}
	}
	recs, err := s.LastRequests(10)
	if err != nil || len(recs) != 3 {
		t.Fatalf("LastRequests: %v %v", recs, err)
	}
	if recs[1].CachedTokens == nil || *recs[1].CachedTokens != 0 || recs[2].CachedTokens != nil {
		t.Errorf("LastRequests zero/none = %v/%v", recs[1].CachedTokens, recs[2].CachedTokens)
	}
}

// A database from before the cache columns existed migrates forward: its old
// row reads nil, the columns match a fresh install, and a row written the way
// an older binary writes it (explicit column list) leaves them NULL after the
// upgrade is applied again.
func TestRequestUsage_MigrateOldSchemaAndDowngradeThenUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "marbor.db")
	buildHistoricalDB(t, dbPath, loadHistoricalDDL(t, "v0.20.0.sql"))
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw, `INSERT INTO request_log (id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts) VALUES ('old','k','m','n',200,5,10,0,'n',0,?)`, time.Now().Unix()-10)
	raw.Close()

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	old, ok, err := s.GetRequest("old")
	if err != nil || !ok || old.CachedTokens != nil || old.InputTokens != nil {
		t.Fatalf("pre-migration row = %+v ok=%v err=%v, want nil cache fields", old, ok, err)
	}
	s.Close()

	// "Downgrade": an older binary inserts with its explicit column list.
	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw, `INSERT INTO request_log (id, key_name, model, node_name, status_code, latency_ms, tokens_used, cost_usd, routed_to, is_cloud, ts, routing_reason, routing_detail, prefill_ms, queue_wait_ms, cloud_ms) VALUES ('mid','k','m','n',200,5,10,0,'n',0,?,'','',0,NULL,NULL)`, time.Now().Unix())
	raw.Close()

	s, err = store.Open(dbPath) // upgrade again
	if err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	defer s.Close()
	mid, ok, err := s.GetRequest("mid")
	if err != nil || !ok || mid.CachedTokens != nil || mid.InputTokens != nil {
		t.Fatalf("downgrade-written row = %+v ok=%v err=%v, want nil cache fields", mid, ok, err)
	}

	fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	a, _ := sql.Open("sqlite", dbPath)
	defer a.Close()
	b, _ := sql.Open("sqlite", fresh.DBPath())
	defer b.Close()
	if got, want := columnNames(t, a, "request_log"), columnNames(t, b, "request_log"); !reflect.DeepEqual(got, want) {
		t.Errorf("upgraded request_log columns %v differ from a fresh install %v", got, want)
	}
}

// The cache-window read is narrow (no routing detail) and returns the newest
// rows quickly even with the table at its 1000-row retention size.
func TestCacheWindowRows_NarrowAndBounded(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Now().Add(-2 * time.Hour)
	// Bulk-load through one raw transaction: AppendRequest trims after every
	// insert, which makes 1000 sequential appends needlessly slow here.
	raw, err := sql.Open("sqlite", s.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		var cached, input any
		if i%2 == 0 {
			cached, input = 1, 2
		}
		_, err := tx.Exec(`INSERT INTO request_log (id, key_name, model, node_name, status_code, ts, routing_detail, cached_tokens, input_tokens) VALUES (?, 'k', 'm', 'n', 200, ?, '{"big":"detail"}', ?, ?)`,
			fmt.Sprintf("r%04d", i), base.Add(time.Duration(i)*time.Second).Unix(), cached, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rows, err := s.CacheWindowRows(1000)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("CacheWindowRows took %v, want a short bounded read", d)
	}
	if len(rows) != 1000 {
		t.Fatalf("rows = %d, want 1000", len(rows))
	}
	if !rows[0].TS.After(rows[999].TS) {
		t.Errorf("rows are not newest first")
	}
	// Newest first, and the narrow columns carry the stored values.
	if rows[0].Model != "m" || rows[0].KeyName != "k" {
		t.Errorf("row 0 model/key = %q/%q, want m/k", rows[0].Model, rows[0].KeyName)
	}
	for i, r := range rows {
		want := 999 - i // row i is insert index 999-i; even indexes carry 1/2
		if want%2 == 0 {
			if r.CachedTokens == nil || *r.CachedTokens != 1 || r.InputTokens == nil || *r.InputTokens != 2 {
				t.Fatalf("row %d split = %v/%v, want 1/2", i, r.CachedTokens, r.InputTokens)
			}
		} else if r.CachedTokens != nil || r.InputTokens != nil {
			t.Fatalf("row %d split = %v/%v, want nil", i, r.CachedTokens, r.InputTokens)
		}
	}
	if rows, err := s.CacheWindowRows(5); err != nil || len(rows) != 5 {
		t.Errorf("limit 5: %d rows err=%v", len(rows), err)
	}
}

// Rows sharing one second-resolution ts come back in the same order every
// time: the window read is newest insert first, and the list read picks the
// same latest inserts.
func TestCacheWindowRowsAndLastRequests_EqualTSOrderIsDeterministic(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "tie.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := time.Now().Truncate(time.Second)
	for i := 0; i < 6; i++ {
		err := s.AppendRequest(store.RequestRecord{ID: fmt.Sprintf("t%d", i), KeyName: "k", Model: fmt.Sprintf("m%d", i), NodeName: "n", StatusCode: 200, TS: ts})
		if err != nil {
			t.Fatal(err)
		}
	}
	for run := 0; run < 3; run++ {
		rows, err := s.CacheWindowRows(3)
		if err != nil || len(rows) != 3 {
			t.Fatalf("CacheWindowRows: %v %v", rows, err)
		}
		if rows[0].Model != "m5" || rows[1].Model != "m4" || rows[2].Model != "m3" {
			t.Errorf("window rows = %v %v %v, want m5 m4 m3 (latest insert first)", rows[0].Model, rows[1].Model, rows[2].Model)
		}
		recs, err := s.LastRequests(3)
		if err != nil || len(recs) != 3 {
			t.Fatalf("LastRequests: %v %v", recs, err)
		}
		// LastRequests returns the newest n in chronological order.
		if recs[0].ID != "t3" || recs[1].ID != "t4" || recs[2].ID != "t5" {
			t.Errorf("LastRequests = %s %s %s, want t3 t4 t5 (the three latest inserts)", recs[0].ID, recs[1].ID, recs[2].ID)
		}
	}
}

// AppendRequest enforces the split invariant at the write boundary: a pair
// that is half set, has no prompt, is negative, exceeds the prompt or the
// per-request cap is stored as NULL for both.
func TestAppendRequest_InvalidSplitStoredAsNull(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "inv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cases := map[string][2]*int64{
		"half-cached": {int64p(1), nil},
		"half-input":  {nil, int64p(5)},
		"zero-input":  {int64p(0), int64p(0)},
		"negative":    {int64p(-1), int64p(5)},
		"over-prompt": {int64p(6), int64p(5)},
		"over-cap":    {int64p(1), int64p(store.MaxCacheSplitTokens + 1)},
	}
	for id, c := range cases {
		if err := s.AppendRequest(store.RequestRecord{ID: id, KeyName: "k", Model: "m", NodeName: "n", StatusCode: 200, TS: time.Now(), CachedTokens: c[0], InputTokens: c[1]}); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.GetRequest(id)
		if err != nil || !ok {
			t.Fatalf("GetRequest %s: %v %v", id, ok, err)
		}
		if got.CachedTokens != nil || got.InputTokens != nil {
			t.Errorf("%s stored %v/%v, want NULL/NULL", id, got.CachedTokens, got.InputTokens)
		}
	}
	good := int64p(5)
	if err := s.AppendRequest(store.RequestRecord{ID: "ok", KeyName: "k", Model: "m", NodeName: "n", StatusCode: 200, TS: time.Now(), CachedTokens: int64p(5), InputTokens: good}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetRequest("ok"); got.CachedTokens == nil || *got.CachedTokens != 5 {
		t.Errorf("cached == input must be kept, got %v", got.CachedTokens)
	}
}

// RequestCacheSplits answers a batch lookup: found rows keep a real 0 and a
// NULL split as nil, missing ids are absent, and a batch larger than one
// query's variable bound is chunked rather than failing.
func TestRequestCacheSplits_BatchAndChunking(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "split.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, c := range []struct {
		id            string
		cached, input *int64
	}{{"val", int64p(3), int64p(10)}, {"zero", int64p(0), int64p(10)}, {"null", nil, nil}} {
		if err := s.AppendRequest(store.RequestRecord{ID: c.id, KeyName: "k", Model: "m", NodeName: "n", StatusCode: 200, TS: now, CachedTokens: c.cached, InputTokens: c.input}); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{"val", "zero", "null", "missing"}
	for i := 0; i < 2500; i++ {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	got, err := s.RequestCacheSplits(ids)
	if err != nil {
		t.Fatalf("RequestCacheSplits over %d ids: %v", len(ids), err)
	}
	if len(got) != 3 {
		t.Fatalf("matched %d ids, want 3: %v", len(got), got)
	}
	if v := got["val"]; v.Cached == nil || *v.Cached != 3 || *v.Input != 10 {
		t.Errorf("val = %+v", v)
	}
	if v := got["zero"]; v.Cached == nil || *v.Cached != 0 {
		t.Errorf("zero = %+v, want a kept 0", v)
	}
	if v, ok := got["null"]; !ok || v.Cached != nil || v.Input != nil {
		t.Errorf("null = %+v ok=%v, want present with nil split", v, ok)
	}
	// A match that sits in a later chunk is still found.
	late := append(make([]string, 0, 1200), make([]string, 1100)...)
	for i := range late {
		late[i] = fmt.Sprintf("pad-%d", i)
	}
	late = append(late, "val")
	if got, err := s.RequestCacheSplits(late); err != nil || got["val"].Input == nil {
		t.Errorf("match in a later chunk lost: %v err=%v", got, err)
	}
	if got, err := s.RequestCacheSplits(nil); err != nil || len(got) != 0 {
		t.Errorf("empty ids = %v err=%v", got, err)
	}
}
