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
		names = append(names, name)
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
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("CacheWindowRows took %v, want well under 2s", d)
	}
	if len(rows) != 1000 {
		t.Fatalf("rows = %d, want 1000", len(rows))
	}
	if !rows[0].TS.After(rows[999].TS) {
		t.Errorf("rows are not newest first")
	}
	// The row type carries exactly five fields: ts, model, key, cached, input.
	if n := reflect.TypeOf(store.CacheWindowRow{}).NumField(); n != 5 {
		t.Errorf("CacheWindowRow has %d fields, want 5 (no routing detail)", n)
	}
	if rows, err := s.CacheWindowRows(5); err != nil || len(rows) != 5 {
		t.Errorf("limit 5: %d rows err=%v", len(rows), err)
	}
}
