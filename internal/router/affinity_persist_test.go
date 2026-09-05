package router

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// TestFlushAffinityThenRestoreRoundTrips covers audit finding #8: a restart
// must not drop every in-flight sticky session. FlushAffinity's snapshot must round-trip through
// RestoreAffinity into a usable in-memory entry.
func TestFlushAffinityThenRestoreRoundTrips(t *testing.T) {
	st := openWarmTestStore(t)

	r := &Router{
		affinity:    make(map[string]*affinityEntry),
		affinityTTL: time.Hour,
	}
	r.SetStore(st)

	entry := &affinityEntry{nodeURL: "http://node-a:11434"}
	entry.lastSeen.Store(time.Now().UnixNano())
	r.affinity["session-1"] = entry

	r.FlushAffinity()

	r2 := &Router{
		affinity:    make(map[string]*affinityEntry),
		affinityTTL: time.Hour,
	}
	r2.SetStore(st)

	n, err := r2.RestoreAffinity()
	if err != nil {
		t.Fatalf("RestoreAffinity: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d entries, want 1", n)
	}
	restored, ok := r2.affinity["session-1"]
	if !ok {
		t.Fatal("session-1 not present after restore")
	}
	if restored.nodeURL != "http://node-a:11434" {
		t.Errorf("restored nodeURL = %q, want %q", restored.nodeURL, "http://node-a:11434")
	}
}

// TestRestoreAffinitySkipsExpiredEntries verifies a session past the TTL
// window at restore time is not resurrected - restoring it would just have
// sweepAffinity delete it on the very next tick, so skipping it up front
// avoids briefly honoring a stale pin.
func TestRestoreAffinitySkipsExpiredEntries(t *testing.T) {
	st := openWarmTestStore(t)
	if err := st.SnapshotAffinity([]store.AffinityRecord{
		{SessionID: "stale", NodeURL: "http://node-a:11434", LastSeen: time.Now().Add(-2 * time.Hour)},
		{SessionID: "fresh", NodeURL: "http://node-b:11434", LastSeen: time.Now()},
	}); err != nil {
		t.Fatalf("SnapshotAffinity: %v", err)
	}

	r := &Router{
		affinity:    make(map[string]*affinityEntry),
		affinityTTL: time.Hour,
	}
	r.SetStore(st)

	n, err := r.RestoreAffinity()
	if err != nil {
		t.Fatalf("RestoreAffinity: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d entries, want 1 (stale entry must be skipped)", n)
	}
	if _, ok := r.affinity["stale"]; ok {
		t.Error("stale session restored, want skipped")
	}
	if _, ok := r.affinity["fresh"]; !ok {
		t.Error("fresh session not restored")
	}
}

// TestFlushAffinityNilStoreIsNoop mirrors TestWarmStateNilStoreIsNoop - a
// Router built directly in tests (no SetStore call) must not panic.
func TestFlushAffinityNilStoreIsNoop(t *testing.T) {
	r := &Router{affinity: map[string]*affinityEntry{}, affinityTTL: time.Hour}
	r.FlushAffinity() // must not panic

	n, err := r.RestoreAffinity()
	if err != nil || n != 0 {
		t.Errorf("RestoreAffinity on nil store = (%d, %v), want (0, nil)", n, err)
	}
}
