package store

import (
	"path/filepath"
	"testing"
)

// TestUpsertKeyRoundTripsLocalOnly guards the P66 local_only field: a key
// created with LocalOnly=true must read back as LocalOnly=true, and default
// (omitted) keys must remain false, matching the DailyUsdCap/MonthlyUsdCap
// zero-value convention already in use for this struct.
func TestUpsertKeyRoundTripsLocalOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "finance", Key: "sk-finance", LocalOnly: true}); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}
	if err := st.UpsertKey(KeyRecord{Name: "default", Key: "sk-default"}); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}

	keys, err := st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	byName := map[string]KeyRecord{}
	for _, k := range keys {
		byName[k.Name] = k
	}
	if !byName["finance"].LocalOnly {
		t.Fatal("expected finance key to round-trip LocalOnly=true")
	}
	if byName["default"].LocalOnly {
		t.Fatal("expected default key to round-trip LocalOnly=false")
	}
}

// TestIncrSpillCounter guards the increment-in-place accounting table: two
// increments to the same (key, served_by) pair must accumulate to 2, not
// overwrite to 1, and distinct served_by values for the same key must be
// tracked independently.
func TestIncrSpillCounter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.IncrSpillCounter("finance", "local"); err != nil {
		t.Fatalf("IncrSpillCounter: %v", err)
	}
	if err := st.IncrSpillCounter("finance", "local"); err != nil {
		t.Fatalf("IncrSpillCounter: %v", err)
	}
	if err := st.IncrSpillCounter("finance", "openai"); err != nil {
		t.Fatalf("IncrSpillCounter: %v", err)
	}
	if err := st.IncrSpillCounter("finance", "blocked"); err != nil {
		t.Fatalf("IncrSpillCounter: %v", err)
	}

	rows, err := st.SpillCounters()
	if err != nil {
		t.Fatalf("SpillCounters: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		if r.KeyName != "finance" {
			t.Fatalf("unexpected key_name in result: %+v", r)
		}
		got[r.ServedBy] = r.Requests
	}
	want := map[string]int64{"local": 2, "openai": 1, "blocked": 1}
	for servedBy, wantCount := range want {
		if got[servedBy] != wantCount {
			t.Fatalf("served_by=%s: got %d requests, want %d (all rows: %+v)", servedBy, got[servedBy], wantCount, rows)
		}
	}
}

// TestDeleteKeyPreservesSpillCounters guards the P66 design decision that
// spill_counters is historical telemetry, not derived state: deleting an API
// key must not delete or rename its prior spill history.
func TestDeleteKeyPreservesSpillCounters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "temp-key", Key: "sk-temp"}); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}
	if err := st.IncrSpillCounter("temp-key", "local"); err != nil {
		t.Fatalf("IncrSpillCounter: %v", err)
	}

	if err := st.DeleteKey("temp-key"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	rows, err := st.SpillCounters()
	if err != nil {
		t.Fatalf("SpillCounters: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.KeyName == "temp-key" && r.ServedBy == "local" && r.Requests == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected temp-key's spill_counters row to survive DeleteKey, got rows: %+v", rows)
	}
}
