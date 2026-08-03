package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDeleteKeyRemovesCounters is the regression guard for the bug where
// DeleteKey only removed the runtime_keys row, leaving the matching
// key_counters row behind. Because the auth reload path restores quota
// state by key name (internal/auth/persist.go), a deleted key's usage
// survived in SQLite and would be reassigned - with stale daily/monthly
// counts - if an operator later created a new key with the same name.
func TestDeleteKeyRemovesCounters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertKey(KeyRecord{Name: "bob", Key: "sk-bob"}); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}
	if err := st.SaveKeyCounters("bob", KeyCounterSnapshot{
		Today: 42, Month: 100, TokensToday: 1000, TokensMonth: 5000, LastReset: time.Now(),
	}); err != nil {
		t.Fatalf("SaveKeyCounters: %v", err)
	}

	counters, err := st.AllKeyCounters()
	if err != nil {
		t.Fatalf("AllKeyCounters (before delete): %v", err)
	}
	if _, ok := counters["bob"]; !ok {
		t.Fatal("setup: expected counter row for bob before delete")
	}

	if err := st.DeleteKey("bob"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	counters, err = st.AllKeyCounters()
	if err != nil {
		t.Fatalf("AllKeyCounters (after delete): %v", err)
	}
	if snap, ok := counters["bob"]; ok {
		t.Fatalf("key_counters row for bob should be deleted with the key, got stale snapshot %+v", snap)
	}
}
