package store_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTestDBAt(t *testing.T, path string) store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return s
}

// TestAppendRequestPersists verifies AppendRequest + LastRequests survive reopen.
func TestAppendRequestPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "req.db")

	ts := time.Unix(1700000000, 0).UTC()
	rec := store.RequestRecord{
		ID:         "req-001",
		KeyName:    "mykey",
		Model:      "llama3",
		NodeName:   "node1",
		StatusCode: 200,
		LatencyMs:  42,
		TokensUsed: 100,
		CostUSD:    0.001,
		RoutedTo:   "http://localhost:11434",
		IsCloud:    false,
		TS:         ts,
	}

	// Write
	s1 := openTestDBAt(t, path)
	if err := s1.AppendRequest(rec); err != nil {
		t.Fatalf("AppendRequest: %v", err)
	}
	s1.Close()

	// Reopen + read
	s2 := openTestDBAt(t, path)
	defer s2.Close()
	recs, err := s2.LastRequests(10)
	if err != nil {
		t.Fatalf("LastRequests: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.ID != rec.ID {
		t.Errorf("ID mismatch: got %q want %q", got.ID, rec.ID)
	}
	if got.KeyName != rec.KeyName {
		t.Errorf("KeyName mismatch: got %q want %q", got.KeyName, rec.KeyName)
	}
	if got.Model != rec.Model {
		t.Errorf("Model mismatch: got %q want %q", got.Model, rec.Model)
	}
	if got.TS.Unix() != rec.TS.Unix() {
		t.Errorf("TS mismatch: got %v want %v", got.TS, rec.TS)
	}
	if got.IsCloud != rec.IsCloud {
		t.Errorf("IsCloud mismatch: got %v want %v", got.IsCloud, rec.IsCloud)
	}
}

// TestKeySpendSince verifies per-key cloud spend is summed correctly,
// excludes local (non-cloud) requests, other keys, and rows before `since`.
func TestKeySpendSince(t *testing.T) {
	s := openTestDB(t)

	base := time.Unix(1_700_000_000, 0).UTC()
	rows := []store.RequestRecord{
		{ID: "r1", KeyName: "alice", CostUSD: 1.00, IsCloud: true, TS: base.Add(1 * time.Hour)},
		{ID: "r2", KeyName: "alice", CostUSD: 2.50, IsCloud: true, TS: base.Add(2 * time.Hour)},
		{ID: "r3", KeyName: "alice", CostUSD: 5.00, IsCloud: false, TS: base.Add(3 * time.Hour)}, // local, excluded
		{ID: "r4", KeyName: "bob", CostUSD: 9.00, IsCloud: true, TS: base.Add(2 * time.Hour)},    // other key, excluded
		{ID: "r5", KeyName: "alice", CostUSD: 3.00, IsCloud: true, TS: base.Add(-1 * time.Hour)}, // before `since`, excluded
	}
	for _, r := range rows {
		if err := s.AppendRequest(r); err != nil {
			t.Fatalf("AppendRequest(%s): %v", r.ID, err)
		}
	}

	got, err := s.KeySpendSince("alice", base)
	if err != nil {
		t.Fatalf("KeySpendSince: %v", err)
	}
	if want := 3.50; got != want {
		t.Errorf("KeySpendSince(alice, base) = %v, want %v", got, want)
	}

	if got, err := s.KeySpendSince("carol", base); err != nil || got != 0 {
		t.Errorf("KeySpendSince(carol, base) = (%v, %v), want (0, nil)", got, err)
	}
}

// TestUpsertDeleteNode verifies UpsertNode + AllNodes + DeleteNode.
func TestUpsertDeleteNode(t *testing.T) {
	s := openTestDB(t)

	vram := int64(8192)
	n := store.NodeRecord{
		Name:        "gpu-node-1",
		URL:         "http://gpu1:11434",
		Runtime:     "ollama",
		VRAMTotalMB: &vram,
	}

	if err := s.UpsertNode(n); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	nodes, err := s.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Name != n.Name {
		t.Errorf("Name mismatch: got %q want %q", nodes[0].Name, n.Name)
	}
	if nodes[0].VRAMTotalMB == nil || *nodes[0].VRAMTotalMB != vram {
		t.Errorf("VRAMTotalMB mismatch")
	}

	if err := s.DeleteNode(n.Name); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	nodes, err = s.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes after delete: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes after delete, got %d", len(nodes))
	}
}

// TestUpsertRevokeKey verifies UpsertKey + AllKeys + RevokeKey.
func TestUpsertRevokeKey(t *testing.T) {
	s := openTestDB(t)

	k := store.KeyRecord{
		Name:         "testkey",
		Key:          "sk-abc123",
		RateLimit:    1000,
		DailyLimit:   5000,
		MonthlyLimit: 100000,
		Models:       []string{"llama3", "mistral"},
		Revoked:      false,
		ExpiresAt:    "2099-01-01T15:04",
	}

	if err := s.UpsertKey(k); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}

	keys, err := s.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Revoked {
		t.Error("expected Revoked=false")
	}
	if len(keys[0].Models) != 2 {
		t.Errorf("expected 2 models, got %d: %v", len(keys[0].Models), keys[0].Models)
	}
	// Regression: ExpiresAt was applied in-memory at creation but never had a
	// column in runtime_keys, so it was silently lost on every restart.
	if keys[0].ExpiresAt != "2099-01-01T15:04" {
		t.Errorf("expected ExpiresAt to survive UpsertKey/AllKeys round-trip, got %q", keys[0].ExpiresAt)
	}

	// Revoke it.
	if err := s.RevokeKey(k.Name); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	keys, err = s.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys after revoke: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key still (revoked, not deleted), got %d", len(keys))
	}
	if !keys[0].Revoked {
		t.Error("expected Revoked=true after RevokeKey")
	}
}

// TestCountersPersist verifies SetCounters + GetCounters survive reopen.
func TestCountersPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "counters.db")

	c := store.Counters{
		LocalRequests: 42,
		CloudRequests: 7,
		TotalTokens:   99000,
		CloudSpentUSD: 1.23,
	}

	s1 := openTestDBAt(t, path)
	if err := s1.SetCounters(c); err != nil {
		t.Fatalf("SetCounters: %v", err)
	}
	s1.Close()

	s2 := openTestDBAt(t, path)
	defer s2.Close()
	got, err := s2.GetCounters()
	if err != nil {
		t.Fatalf("GetCounters: %v", err)
	}
	if got.LocalRequests != c.LocalRequests {
		t.Errorf("LocalRequests: got %d want %d", got.LocalRequests, c.LocalRequests)
	}
	if got.CloudSpentUSD != c.CloudSpentUSD {
		t.Errorf("CloudSpentUSD: got %f want %f", got.CloudSpentUSD, c.CloudSpentUSD)
	}
}

// TestKeyCounters verifies SaveKeyCounters + AllKeyCounters.
func TestKeyCounters(t *testing.T) {
	s := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	snap := store.KeyCounterSnapshot{
		Today:       5,
		Month:       50,
		TokensToday: 1000,
		TokensMonth: 10000,
		LastReset:   now,
	}

	if err := s.SaveKeyCounters("mykey", snap); err != nil {
		t.Fatalf("SaveKeyCounters: %v", err)
	}

	all, err := s.AllKeyCounters()
	if err != nil {
		t.Fatalf("AllKeyCounters: %v", err)
	}
	got, ok := all["mykey"]
	if !ok {
		t.Fatal("key 'mykey' not found")
	}
	if got.Today != snap.Today {
		t.Errorf("Today: got %d want %d", got.Today, snap.Today)
	}
	if got.TokensMonth != snap.TokensMonth {
		t.Errorf("TokensMonth: got %d want %d", got.TokensMonth, snap.TokensMonth)
	}
	if got.LastReset.Unix() != snap.LastReset.Unix() {
		t.Errorf("LastReset: got %v want %v", got.LastReset, snap.LastReset)
	}
}

// TestNodeDrainStates verifies SetNodeDrain + NodeDrainStates.
func TestNodeDrainStates(t *testing.T) {
	s := openTestDB(t)

	if err := s.SetNodeDrain("node1", true, "manual"); err != nil {
		t.Fatalf("SetNodeDrain: %v", err)
	}
	if err := s.SetNodeDrain("node2", false, ""); err != nil {
		t.Fatalf("SetNodeDrain: %v", err)
	}

	states, err := s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates: %v", err)
	}
	if !states["node1"].Draining {
		t.Error("node1 should be draining")
	}
	if states["node1"].Reason != "manual" {
		t.Errorf("node1 reason = %q, want manual", states["node1"].Reason)
	}
	if states["node2"].Draining {
		t.Error("node2 should not be draining")
	}

	// Update node1 to not draining.
	if err := s.SetNodeDrain("node1", false, ""); err != nil {
		t.Fatalf("SetNodeDrain update: %v", err)
	}
	states, err = s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates after update: %v", err)
	}
	if states["node1"].Draining {
		t.Error("node1 should no longer be draining")
	}
}

// TestPredictiveHistory verifies AppendPredictiveTransition + PredictiveHistory
// survive across a fresh Open() (restart simulation).
func TestPredictiveHistory(t *testing.T) {
	s := openTestDB(t)

	base := time.Now().UTC().Truncate(time.Second)
	if err := s.AppendPredictiveTransition("", "llama3", base); err != nil {
		t.Fatalf("AppendPredictiveTransition: %v", err)
	}
	if err := s.AppendPredictiveTransition("llama3", "mistral", base.Add(time.Minute)); err != nil {
		t.Fatalf("AppendPredictiveTransition: %v", err)
	}

	hist, err := s.PredictiveHistory()
	if err != nil {
		t.Fatalf("PredictiveHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("len(hist) = %d, want 2", len(hist))
	}
	if hist[0].FromModel != "" || hist[0].ToModel != "llama3" {
		t.Errorf("hist[0] = %+v, want FromModel=\"\" ToModel=llama3", hist[0])
	}
	if hist[1].FromModel != "llama3" || hist[1].ToModel != "mistral" {
		t.Errorf("hist[1] = %+v, want FromModel=llama3 ToModel=mistral", hist[1])
	}
	if !hist[0].Timestamp.Equal(base) {
		t.Errorf("hist[0].Timestamp = %v, want %v", hist[0].Timestamp, base)
	}
}

// TestNodeOverrides verifies UpsertNodeOverride + NodeOverrides.
func TestNodeOverrides(t *testing.T) {
	s := openTestDB(t)

	vram := int64(16384)
	gpu := "NVIDIA RTX 4090"
	rt := "vllm"

	if err := s.UpsertNodeOverride("node1", &vram, &gpu, &rt, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}

	ovs, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov, ok := ovs["node1"]
	if !ok {
		t.Fatal("node1 override not found")
	}
	if ov.VRAMTotalMB == nil || *ov.VRAMTotalMB != vram {
		t.Errorf("VRAMTotalMB mismatch")
	}
	if ov.GPUModel == nil || *ov.GPUModel != gpu {
		t.Errorf("GPUModel mismatch")
	}
	if ov.Runtime == nil || *ov.Runtime != rt {
		t.Errorf("Runtime mismatch")
	}
	if ov.GPUIndices != nil {
		t.Errorf("GPUIndices = %v, want nil (never declared)", ov.GPUIndices)
	}
	if ov.MaxInFlight != nil {
		t.Errorf("MaxInFlight = %v, want nil (never declared)", ov.MaxInFlight)
	}
	if ov.TLSFingerprint != nil {
		t.Errorf("TLSFingerprint = %v, want nil (never declared)", ov.TLSFingerprint)
	}
}

// TestNodeOverrides_MaxInFlight verifies the P64 per-node in-flight cap
// override column: roundtrips through UpsertNodeOverride/NodeOverrides,
// survives a merge update that only touches an unrelated field (gpu_model),
// and distinguishes "never declared" (nil) from an explicit 0.
func TestNodeOverrides_MaxInFlight(t *testing.T) {
	s := openTestDB(t)

	cap1 := 8
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, nil, &cap1, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}
	ovs, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov, ok := ovs["node1"]
	if !ok {
		t.Fatal("node1 override not found")
	}
	if ov.MaxInFlight == nil || *ov.MaxInFlight != cap1 {
		t.Fatalf("MaxInFlight = %v, want %d", ov.MaxInFlight, cap1)
	}

	// A merge update that only touches gpu_model must not clobber the
	// previously-set max_in_flight (same merge discipline as vram_total_mb).
	gpu := "NVIDIA RTX 4090"
	if err := s.UpsertNodeOverride("node1", nil, &gpu, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (merge): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov = ovs["node1"]
	if ov.MaxInFlight == nil || *ov.MaxInFlight != cap1 {
		t.Errorf("MaxInFlight after unrelated merge = %v, want preserved %d", ov.MaxInFlight, cap1)
	}

	// An explicit 0 (clearing back to "use global default") must round-trip
	// as a non-nil pointer to 0, distinct from "never declared" (nil).
	cap0 := 0
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, nil, &cap0, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (clear): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov = ovs["node1"]
	if ov.MaxInFlight == nil || *ov.MaxInFlight != 0 {
		t.Errorf("MaxInFlight after explicit clear = %v, want non-nil 0", ov.MaxInFlight)
	}
}

// TestNodeOverrides_GPUIndices verifies the P75 Gap B/C declared-GPU-scope
// column: roundtrips through UpsertNodeOverride/NodeOverrides, survives a
// merge update that only touches an unrelated field (gpu_model), and can be
// explicitly cleared back to "nothing declared" with a non-nil empty slice.
func TestNodeOverrides_GPUIndices(t *testing.T) {
	s := openTestDB(t)

	indices := []int{0, 1}
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, &indices, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}

	ovs, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov, ok := ovs["node1"]
	if !ok {
		t.Fatal("node1 override not found")
	}
	if ov.GPUIndices == nil || len(*ov.GPUIndices) != 2 || (*ov.GPUIndices)[0] != 0 || (*ov.GPUIndices)[1] != 1 {
		t.Fatalf("GPUIndices = %v, want [0 1]", ov.GPUIndices)
	}

	// A merge update touching only gpu_model must not clobber the earlier
	// gpu_indices declaration - same discipline as vram_total_mb/runtime.
	gpu := "NVIDIA RTX 4090"
	if err := s.UpsertNodeOverride("node1", nil, &gpu, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (merge): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides (after merge): %v", err)
	}
	ov = ovs["node1"]
	if ov.GPUIndices == nil || len(*ov.GPUIndices) != 2 {
		t.Fatalf("GPUIndices after unrelated merge = %v, want unchanged [0 1]", ov.GPUIndices)
	}

	// An explicit non-nil empty slice clears the declaration back to "no
	// scoping applied" - scopeGPUsToDeclared treats any len==0 slice as
	// "nothing declared", so the round-tripped value only needs to be empty,
	// not nil, to have the intended effect.
	empty := []int{}
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, &empty, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (clear): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides (after clear): %v", err)
	}
	ov = ovs["node1"]
	if ov.GPUIndices == nil || len(*ov.GPUIndices) != 0 {
		t.Errorf("GPUIndices after clear = %v, want empty", ov.GPUIndices)
	}
}

// TestNodeOverrides_TLSFingerprint verifies the P24 tls_fingerprint column:
// round-trips through UpsertNodeOverride/NodeOverrides, survives a merge
// update that only touches an unrelated field, and can be explicitly cleared
// with a non-nil empty string. See .local/specs/node-agent-tls.md section 3.
func TestNodeOverrides_TLSFingerprint(t *testing.T) {
	s := openTestDB(t)

	fp := "SHA256:aa:bb:cc:dd"
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, nil, nil, &fp, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}
	ovs, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov, ok := ovs["node1"]
	if !ok {
		t.Fatal("node1 override not found")
	}
	if ov.TLSFingerprint == nil || *ov.TLSFingerprint != fp {
		t.Fatalf("TLSFingerprint = %v, want %q", ov.TLSFingerprint, fp)
	}

	// A merge update touching only gpu_model must not clobber the previously
	// pinned fingerprint - same merge discipline as every other override
	// column, and the whole reason this column round-trips through a
	// read-merge instead of a blanket overwrite.
	gpu := "NVIDIA RTX 4090"
	if err := s.UpsertNodeOverride("node1", nil, &gpu, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (merge): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides (after merge): %v", err)
	}
	ov = ovs["node1"]
	if ov.TLSFingerprint == nil || *ov.TLSFingerprint != fp {
		t.Errorf("TLSFingerprint after unrelated merge = %v, want preserved %q", ov.TLSFingerprint, fp)
	}

	// Explicit clear (non-nil empty string) resets the pin to nil/NULL - this
	// is the "reset pin" path (spec section 2), distinct from "never declared".
	empty := ""
	if err := s.UpsertNodeOverride("node1", nil, nil, nil, nil, nil, &empty, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride (clear): %v", err)
	}
	ovs, err = s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides (after clear): %v", err)
	}
	ov = ovs["node1"]
	if ov.TLSFingerprint != nil {
		t.Errorf("TLSFingerprint after clear = %v, want nil", ov.TLSFingerprint)
	}
}

// TestNodeOverrides_UpsertDoesNotClobberUnknownColumn is the regression test
// for the P24/.local/core/P24-TLS-DESIGN.md section 10b downgrade-safety fix.
// It directly proves UpsertNodeOverride's write primitive (INSERT ... ON
// CONFLICT DO UPDATE SET, naming only the columns this function's Go
// signature carries) never clobbers a column outside that list - unlike the
// prior INSERT OR REPLACE shape, which deletes and reinserts the whole row
// and would silently NULL any column the executing statement doesn't name.
// Simulates "an older binary's compiled UpsertNodeOverride, which only knows
// about max_in_flight and doesn't carry a tls_fingerprint param at all,
// writes to a row a newer binary already pinned" via a raw SQL statement
// against the same on-disk file (same technique as
// TestOpenUpgradesPreCapRuntimeKeysSchema above), without needing an actual
// second compiled binary.
func TestNodeOverrides_UpsertDoesNotClobberUnknownColumn(t *testing.T) {
	// Uses its own os.MkdirTemp instead of t.TempDir(): this test opens and
	// closes three separate connections to one file (real Store x2, raw x1)
	// to simulate an older binary's write, and on Windows the OS can be a
	// beat slow to release the WAL-mode file handle after Close() returns -
	// t.TempDir()'s cleanup treats that as a fatal test failure, which isn't
	// what this test is checking. Best-effort removal (log, don't fail) is
	// the correct behavior for a cleanup race that isn't the property under
	// test.
	dir, err := os.MkdirTemp("", "downgrade-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("best-effort temp dir cleanup failed (non-fatal): %v", err)
		}
	})
	path := filepath.Join(dir, "downgrade.db")

	// Connection 1: seed the pinned fingerprint via the real Store, then
	// close it fully before any other connection touches the file - strictly
	// sequential connection lifecycles, same discipline as
	// TestOpenUpgradesPreCapRuntimeKeysSchema above, so no two connections
	// ever hold the file open at once (avoids platform-specific file-lock
	// flakiness; this is a connection-lifecycle detail, not part of what the
	// test proves).
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	fp := "SHA256:11:22:33:44"
	if err := s1.UpsertNodeOverride("node1", nil, nil, nil, nil, nil, &fp, nil, nil); err != nil {
		t.Fatalf("seed UpsertNodeOverride: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}

	// Connection 2: simulate an older binary's UpsertNodeOverride - same
	// read-merge-then-targeted-write shape, but its compiled Go signature
	// never had a tls_fingerprint param to read or write in the first place,
	// so its own generated SQL simply never names that column. Uses the same
	// WAL/busy_timeout pragma DSN as store.Open (sqlite.go) - the DB file's
	// journal_mode is already WAL from connection 1, and reopening without
	// matching pragmas caused a Windows-only file-lock flake at cleanup.
	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	cap1 := 4
	_, execErr := raw.Exec(
		`INSERT INTO node_overrides (name, max_in_flight) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET max_in_flight = excluded.max_in_flight`,
		"node1", cap1,
	)
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatalf("close raw db: %v", closeErr)
	}
	if execErr != nil {
		t.Fatalf("simulated older-binary upsert: %v", execErr)
	}

	// Connection 3: reopen the real Store to read back the result.
	s2 := openTestDBAt(t, path)
	ovs, err := s2.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	ov := ovs["node1"]
	if ov.TLSFingerprint == nil || *ov.TLSFingerprint != fp {
		t.Fatalf("TLSFingerprint after older-binary-shaped write = %v, want preserved %q (this would be nil under the old INSERT OR REPLACE shape)", ov.TLSFingerprint, fp)
	}
	if ov.MaxInFlight == nil || *ov.MaxInFlight != cap1 {
		t.Fatalf("MaxInFlight = %v, want %d", ov.MaxInFlight, cap1)
	}
}

// TestNopStore verifies NopStore satisfies the interface with no panics.
// TestHourlyBucketAccumulates verifies that repeated per-request writes to the
// same hour sum (not clobber) so cumulative totals survive a restart. This is
// the regression guard for the INSERT-OR-REPLACE bug that persisted only the
// last request's counts.
func TestHourlyBucketAccumulates(t *testing.T) {
	s := openTestDB(t)
	hour := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)

	// Simulate 3 local requests in the same hour, each a delta of 1.
	for i := 0; i < 3; i++ {
		if err := s.UpsertHourlyBucket(store.HourlyBucket{Hour: hour, LocalRequests: 1, Tokens: 100}); err != nil {
			t.Fatalf("UpsertHourlyBucket: %v", err)
		}
	}
	buckets, err := s.HourlyBuckets(hour.Add(-time.Hour))
	if err != nil {
		t.Fatalf("HourlyBuckets: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(buckets))
	}
	if buckets[0].LocalRequests != 3 || buckets[0].Tokens != 300 {
		t.Errorf("bucket = {local:%d tokens:%d}, want {local:3 tokens:300}",
			buckets[0].LocalRequests, buckets[0].Tokens)
	}

	// Model stats accumulate the same way.
	for i := 0; i < 4; i++ {
		if err := s.UpsertModelStat(store.ModelStat{Model: "llama3", Requests: 1, Tokens: 50}); err != nil {
			t.Fatalf("UpsertModelStat: %v", err)
		}
	}
	stats, err := s.AllModelStats()
	if err != nil {
		t.Fatalf("AllModelStats: %v", err)
	}
	if len(stats) != 1 || stats[0].Requests != 4 || stats[0].Tokens != 200 {
		t.Errorf("model stats = %+v, want one row {requests:4 tokens:200}", stats)
	}
}

func TestNopStore(t *testing.T) {
	var s store.Store = store.NopStore{}

	if err := s.AppendRequest(store.RequestRecord{}); err != nil {
		t.Errorf("NopStore AppendRequest: %v", err)
	}
	recs, err := s.LastRequests(10)
	if err != nil || recs != nil {
		t.Errorf("NopStore LastRequests: recs=%v err=%v", recs, err)
	}
	if err := s.SetCounters(store.Counters{}); err != nil {
		t.Errorf("NopStore SetCounters: %v", err)
	}
	c, err := s.GetCounters()
	if err != nil || c != (store.Counters{}) {
		t.Errorf("NopStore GetCounters: %v %v", c, err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("NopStore Close: %v", err)
	}
}

// TestWarmStatePersistsAndRestores verifies the warm-state residency map
// survives a reopen: RecordWarmLoad accumulates load_count, SnapshotWarmState
// refreshes residency without bumping it, and last_used round-trips.
func TestWarmStatePersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warm.db")
	used := time.Unix(1700000000, 0).UTC()

	s := openTestDBAt(t, path)
	// Two loads of the same pair -> load_count should reach 2.
	if err := s.RecordWarmLoad(store.WarmStateRecord{Model: "llama3", Node: "n1", LastUsed: used, VRAMBytes: 4096}); err != nil {
		t.Fatalf("RecordWarmLoad 1: %v", err)
	}
	if err := s.RecordWarmLoad(store.WarmStateRecord{Model: "llama3", Node: "n1", LastUsed: used, VRAMBytes: 5000}); err != nil {
		t.Fatalf("RecordWarmLoad 2: %v", err)
	}
	// A different pair, never used (zero time).
	if err := s.RecordWarmLoad(store.WarmStateRecord{Model: "mistral", Node: "n2", VRAMBytes: 8000}); err != nil {
		t.Fatalf("RecordWarmLoad mistral: %v", err)
	}
	// Snapshot must refresh vram/last_used but NOT bump load_count.
	if err := s.SnapshotWarmState(store.WarmStateRecord{Model: "llama3", Node: "n1", LastUsed: used, VRAMBytes: 6000}); err != nil {
		t.Fatalf("SnapshotWarmState: %v", err)
	}
	s.Close()

	// Reopen and verify state survived.
	s2 := openTestDBAt(t, path)
	defer s2.Close()
	rows, err := s2.AllWarmState()
	if err != nil {
		t.Fatalf("AllWarmState: %v", err)
	}
	got := map[string]store.WarmStateRecord{}
	for _, w := range rows {
		got[w.Model+"@"+w.Node] = w
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), rows)
	}
	l := got["llama3@n1"]
	if l.LoadCount != 2 {
		t.Errorf("llama3 load_count = %d, want 2", l.LoadCount)
	}
	if l.VRAMBytes != 6000 {
		t.Errorf("llama3 vram = %d, want 6000 (snapshot refresh)", l.VRAMBytes)
	}
	if !l.LastUsed.Equal(used) {
		t.Errorf("llama3 last_used = %v, want %v", l.LastUsed, used)
	}
	m := got["mistral@n2"]
	if m.LoadCount != 1 {
		t.Errorf("mistral load_count = %d, want 1", m.LoadCount)
	}
	if !m.LastUsed.IsZero() {
		t.Errorf("mistral last_used = %v, want zero", m.LastUsed)
	}
}

// TestWarmStateDeletes verifies per-pair and per-node deletion.
func TestWarmStateDeletes(t *testing.T) {
	s := openTestDB(t)
	for _, r := range []store.WarmStateRecord{
		{Model: "a", Node: "n1"}, {Model: "b", Node: "n1"}, {Model: "a", Node: "n2"},
	} {
		if err := s.RecordWarmLoad(r); err != nil {
			t.Fatalf("RecordWarmLoad: %v", err)
		}
	}
	if err := s.DeleteWarmState("a", "n1"); err != nil {
		t.Fatalf("DeleteWarmState: %v", err)
	}
	rows, _ := s.AllWarmState()
	if len(rows) != 2 {
		t.Fatalf("after single delete want 2, got %d", len(rows))
	}
	if err := s.DeleteWarmStateByNode("n1"); err != nil {
		t.Fatalf("DeleteWarmStateByNode: %v", err)
	}
	rows, _ = s.AllWarmState()
	if len(rows) != 1 || rows[0].Node != "n2" {
		t.Fatalf("after node delete want 1 row on n2, got %+v", rows)
	}
}

// TestNopStoreWarmState verifies the no-op store satisfies the warm-state API.
func TestNopStoreWarmState(t *testing.T) {
	var s store.Store = store.NopStore{}
	if err := s.RecordWarmLoad(store.WarmStateRecord{}); err != nil {
		t.Errorf("NopStore RecordWarmLoad: %v", err)
	}
	if err := s.SnapshotWarmState(store.WarmStateRecord{}); err != nil {
		t.Errorf("NopStore SnapshotWarmState: %v", err)
	}
	if err := s.DeleteWarmState("m", "n"); err != nil {
		t.Errorf("NopStore DeleteWarmState: %v", err)
	}
	if err := s.DeleteWarmStateByNode("n"); err != nil {
		t.Errorf("NopStore DeleteWarmStateByNode: %v", err)
	}
	rows, err := s.AllWarmState()
	if err != nil || rows != nil {
		t.Errorf("NopStore AllWarmState: rows=%v err=%v", rows, err)
	}
}

// TestQueryAuditLogSubstringFilters verifies key and node filters match on
// substring (like the model filter), not exact string equality. Regression
// for the Requests page filter bar silently returning zero rows on partial
// key/node input.
func TestQueryAuditLogSubstringFilters(t *testing.T) {
	s := openTestDB(t)

	entries := []store.AuditEntry{
		{RequestID: "r1", KeyName: "prod-api-key", Model: "llama3", Node: "gpu-node-01", Status: "200", Time: time.Now()},
		{RequestID: "r2", KeyName: "staging-key", Model: "llama3", Node: "gpu-node-02", Status: "200", Time: time.Now()},
	}
	for _, e := range entries {
		if err := s.AppendAuditLog(e); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}

	got, err := s.QueryAuditLog(store.AuditQuery{Limit: 10, Key: "prod"})
	if err != nil {
		t.Fatalf("QueryAuditLog(Key=prod): %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "r1" {
		t.Fatalf("Key substring filter: got %+v, want single match r1", got)
	}

	got, err = s.QueryAuditLog(store.AuditQuery{Limit: 10, Node: "node-02"})
	if err != nil {
		t.Fatalf("QueryAuditLog(Node=node-02): %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "r2" {
		t.Fatalf("Node substring filter: got %+v, want single match r2", got)
	}
}

// TestAuditLogRoutingReasonRoundTrip verifies routing_reason survives a real
// AppendAuditLog -> SQLite -> QueryAuditLog round trip (P41 code-review fix:
// Requests.tsx reads the audit_log list, not request_log, so this field must
// persist and query back through the actual audit_log column, not just live
// on the in-memory struct).
func TestAuditLogRoutingReasonRoundTrip(t *testing.T) {
	s := openTestDB(t)

	entries := []store.AuditEntry{
		{RequestID: "r1", Model: "llama3", Node: "gpu-node-01", Status: "200", Time: time.Now(), RoutingReason: "session_affinity"},
		{RequestID: "r2", Model: "llama3", Node: "gpu-node-02", Status: "200", Time: time.Now(), RoutingReason: "score_based"},
		{RequestID: "r3", Model: "llama3", Node: "gpu-node-03", Status: "200", Time: time.Now()}, // no decision (e.g. cloud fallback)
	}
	for _, e := range entries {
		if err := s.AppendAuditLog(e); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}

	got, err := s.QueryAuditLog(store.AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("QueryAuditLog: got %d rows, want 3", len(got))
	}

	byID := make(map[string]string, len(got))
	for _, e := range got {
		byID[e.RequestID] = e.RoutingReason
	}
	if byID["r1"] != "session_affinity" {
		t.Errorf("r1 routing_reason = %q, want session_affinity", byID["r1"])
	}
	if byID["r2"] != "score_based" {
		t.Errorf("r2 routing_reason = %q, want score_based", byID["r2"])
	}
	if byID["r3"] != "" {
		t.Errorf("r3 routing_reason = %q, want empty (no decision recorded)", byID["r3"])
	}
}

// TestPruneAuditLog verifies time-based retention: rows older than the
// window are deleted, recent rows survive, and retentionDays <= 0 is a
// no-op (the admin's explicit "keep forever" choice), not "delete all".
func TestPruneAuditLog(t *testing.T) {
	s := openTestDB(t)

	old := store.AuditEntry{RequestID: "old", Model: "llama3", Status: "200", Time: time.Now().Add(-60 * 24 * time.Hour)}
	recent := store.AuditEntry{RequestID: "recent", Model: "llama3", Status: "200", Time: time.Now()}
	for _, e := range []store.AuditEntry{old, recent} {
		if err := s.AppendAuditLog(e); err != nil {
			t.Fatalf("AppendAuditLog: %v", err)
		}
	}

	if err := s.PruneAuditLog(0); err != nil {
		t.Fatalf("PruneAuditLog(0): %v", err)
	}
	got, err := s.QueryAuditLog(store.AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PruneAuditLog(0) must be a no-op (forever), got %d rows", len(got))
	}

	if err := s.PruneAuditLog(30); err != nil {
		t.Fatalf("PruneAuditLog(30): %v", err)
	}
	got, err = s.QueryAuditLog(store.AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "recent" {
		t.Fatalf("PruneAuditLog(30): got %+v, want only the recent row", got)
	}
}

// TestPruneSystemAuditLog mirrors TestPruneAuditLog for the separate admin
// action trail (system_audit_log), which has its own independent retention
// setting defaulting to 0 (forever) rather than audit_log's 30 days.
func TestPruneSystemAuditLog(t *testing.T) {
	s := openTestDB(t)

	old := store.SystemAuditEntry{Username: "admin", Action: "old-action", Time: time.Now().Add(-400 * 24 * time.Hour)}
	recent := store.SystemAuditEntry{Username: "admin", Action: "recent-action", Time: time.Now()}
	for _, e := range []store.SystemAuditEntry{old, recent} {
		if err := s.AppendSystemAuditLog(e); err != nil {
			t.Fatalf("AppendSystemAuditLog: %v", err)
		}
	}

	if err := s.PruneSystemAuditLog(0); err != nil {
		t.Fatalf("PruneSystemAuditLog(0): %v", err)
	}
	got, err := s.QuerySystemAuditLog(10)
	if err != nil {
		t.Fatalf("QuerySystemAuditLog: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PruneSystemAuditLog(0) must be a no-op (forever), got %d rows", len(got))
	}

	if err := s.PruneSystemAuditLog(365); err != nil {
		t.Fatalf("PruneSystemAuditLog(365): %v", err)
	}
	got, err = s.QuerySystemAuditLog(10)
	if err != nil {
		t.Fatalf("QuerySystemAuditLog: %v", err)
	}
	if len(got) != 1 || got[0].Action != "recent-action" {
		t.Fatalf("PruneSystemAuditLog(365): got %+v, want only the recent row", got)
	}
}

// TestQuerySystemAuditLogFilteredKindPagingReturnsFullPage is a regression
// test for a pagination-undercount bug: when a kind filter maps to actions
// that make up a minority of rows, and QuerySystemAuditLogFiltered fetches
// exactly Limit raw SQL rows before checking kind, most of the raw page can
// be discarded by the Go-side kind check - returning far fewer than Limit
// entries even though enough matching rows exist further back. The fix
// overfetches and loops (bounded by maxSystemAuditScan) until Limit matching
// entries are found or the table is exhausted.
func TestQuerySystemAuditLogFilteredKindPagingReturnsFullPage(t *testing.T) {
	s := openTestDB(t)

	// 20 entries, newest first: every 4th one (i=0,4,8,12,16) is a "drain"
	// kind action; the rest are "node" kind. A naive single-page SQL LIMIT=5
	// fetch (ordered by ts DESC) would only capture i=0..4 raw rows, of
	// which just 2 (i=0, i=4) match kind=drain - undercounting the 5 that
	// actually exist.
	base := time.Now()
	for i := 0; i < 20; i++ {
		action := "add_node"
		if i%4 == 0 {
			action = "drain_node"
		}
		e := store.SystemAuditEntry{
			Username: "admin",
			Action:   action,
			Time:     base.Add(-time.Duration(i) * time.Minute),
		}
		if err := s.AppendSystemAuditLog(e); err != nil {
			t.Fatalf("AppendSystemAuditLog(i=%d): %v", i, err)
		}
	}

	got, err := s.QuerySystemAuditLogFiltered(store.SystemAuditFilter{Kind: "drain", Limit: 5})
	if err != nil {
		t.Fatalf("QuerySystemAuditLogFiltered: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("QuerySystemAuditLogFiltered(kind=drain, limit=5) returned %d entries, want 5 (all matching drain_node rows)", len(got))
	}
	for _, e := range got {
		if e.Action != "drain_node" {
			t.Fatalf("QuerySystemAuditLogFiltered(kind=drain) returned non-drain action %q", e.Action)
		}
	}
}

// TestQuerySystemAuditLogFilteredSameSecondTieWithinKindPaging is a
// regression test for a pagination undercount bug: ts is stored via
// time.RFC3339 (1-second resolution), so two rows created in the same
// second have an identical ts. QuerySystemAuditLogFiltered's internal
// overfetch loop (used when a kind filter is active, see
// TestQuerySystemAuditLogFilteredKindPagingReturnsFullPage) advanced its
// cursor with "ts < lastSeenTs" - exclusive on ts alone - so a matching row
// sharing its ts with the last row of a raw SQL page was silently skipped
// on the next internal iteration too, since it is "== cursor", not
// "< cursor". The fix adds the autoincrement id as a tiebreaker so every
// row is visited exactly once across iterations.
func TestQuerySystemAuditLogFilteredSameSecondTieWithinKindPaging(t *testing.T) {
	s := openTestDB(t)

	base := time.Now().Truncate(time.Hour)
	// Six rows, inserted oldest-ts first. Only id=2 is a "drain" kind
	// action, and it shares its ts with id=3 (a "node" kind action) -
	// id=3 ranks just ahead of it (ts DESC, id DESC) as the last row of
	// the first raw SQL page (queryLimit = fetchLimit*4 = 4 rows), so
	// id=2 only surfaces on the internal loop's second iteration.
	entries := []struct {
		action string
		offset time.Duration
	}{
		{"add_node", 0 * time.Second},   // id=1, ts=base+0s
		{"drain_node", 1 * time.Second}, // id=2, ts=base+1s (tied with id=3)
		{"add_node", 1 * time.Second},   // id=3, ts=base+1s (tied with id=2)
		{"add_node", 2 * time.Second},   // id=4, ts=base+2s
		{"add_node", 3 * time.Second},   // id=5, ts=base+3s
		{"add_node", 4 * time.Second},   // id=6, ts=base+4s (newest)
	}
	for i, e := range entries {
		entry := store.SystemAuditEntry{
			Username: "admin",
			Action:   e.action,
			Target:   fmt.Sprintf("row-%d", i+1),
			Time:     base.Add(e.offset),
		}
		if err := s.AppendSystemAuditLog(entry); err != nil {
			t.Fatalf("AppendSystemAuditLog(row-%d): %v", i+1, err)
		}
	}

	got, err := s.QuerySystemAuditLogFiltered(store.SystemAuditFilter{Kind: "drain", Limit: 1})
	if err != nil {
		t.Fatalf("QuerySystemAuditLogFiltered: %v", err)
	}
	if len(got) != 1 || got[0].Action != "drain_node" {
		t.Fatalf("QuerySystemAuditLogFiltered(kind=drain, limit=1) = %+v, want the single drain_node row (same-second tie must not be skipped)", got)
	}
}

// TestQuerySystemAuditLogFilteredUsernameContains verifies the Username
// filter matches anywhere in the username (contains), consistent with the
// Target and SourceIP filters, not just as a prefix.
func TestQuerySystemAuditLogFilteredUsernameContains(t *testing.T) {
	s := openTestDB(t)

	for _, u := range []string{"alice-admin", "bob"} {
		e := store.SystemAuditEntry{Username: u, Action: "add_node", Time: time.Now()}
		if err := s.AppendSystemAuditLog(e); err != nil {
			t.Fatalf("AppendSystemAuditLog(%s): %v", u, err)
		}
	}

	got, err := s.QuerySystemAuditLogFiltered(store.SystemAuditFilter{Username: "admin", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySystemAuditLogFiltered: %v", err)
	}
	if len(got) != 1 || got[0].Username != "alice-admin" {
		t.Fatalf("QuerySystemAuditLogFiltered(username=admin) = %+v, want only alice-admin (substring match)", got)
	}
}

// TestSetCloudProviderPrioritiesRenumbersInOrder verifies that
// SetCloudProviderPriorities renumbers providers so AllCloudProviders (which
// orders by priority DESC, name ASC) reflects the caller's desired order,
// highest priority first.
func TestSetCloudProviderPrioritiesRenumbersInOrder(t *testing.T) {
	st := openTestDB(t)
	for _, name := range []string{"a", "b", "c"} {
		if err := st.UpsertCloudProvider(store.CloudProviderRecord{Name: name, Provider: "openai", BaseURL: "https://x", Enabled: true}); err != nil {
			t.Fatalf("UpsertCloudProvider(%s): %v", name, err)
		}
	}
	// New desired order: c first (highest priority), then a, then b.
	if err := st.SetCloudProviderPriorities([]string{"c", "a", "b"}); err != nil {
		t.Fatalf("SetCloudProviderPriorities: %v", err)
	}
	got, err := st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders: %v", err)
	}
	if len(got) != 3 || got[0].Name != "c" || got[1].Name != "a" || got[2].Name != "b" {
		t.Fatalf("AllCloudProviders() order = %v, want [c a b]", got)
	}
}

// TestSetCloudProviderPrioritiesLeavesOmittedProvidersUntouched verifies that
// providers whose names are not present in the order slice retain their
// existing priority - they are not modified by SetCloudProviderPriorities.
func TestSetCloudProviderPrioritiesLeavesOmittedProvidersUntouched(t *testing.T) {
	st := openTestDB(t)
	// Seed three providers: a, b, c with distinct initial priorities.
	providers := []store.CloudProviderRecord{
		{Name: "a", Provider: "openai", BaseURL: "https://x", Enabled: true},
		{Name: "b", Provider: "openai", BaseURL: "https://x", Enabled: true},
		{Name: "c", Provider: "openai", BaseURL: "https://x", Enabled: true},
	}
	for _, cp := range providers {
		if err := st.UpsertCloudProvider(cp); err != nil {
			t.Fatalf("UpsertCloudProvider(%s): %v", cp.Name, err)
		}
	}

	// Verify initial state and set a distinct priority for "c".
	all, err := st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders (initial): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(all))
	}

	// Find provider "c" and remember its initial priority.
	var cInitialPriority int
	for _, p := range all {
		if p.Name == "c" {
			cInitialPriority = p.Priority
			break
		}
	}

	// Perform a partial reorder: only "b" and "a", omitting "c".
	// This should renumber b and a but leave c untouched.
	if err := st.SetCloudProviderPriorities([]string{"b", "a"}); err != nil {
		t.Fatalf("SetCloudProviderPriorities: %v", err)
	}

	// Verify the result.
	got, err := st.AllCloudProviders()
	if err != nil {
		t.Fatalf("AllCloudProviders (after): %v", err)
	}

	// Expect AllCloudProviders to order by priority DESC, name ASC.
	// Since we set order ["b", "a"], "b" should have priority 2 and "a" priority 1.
	// "c" should still have its initial priority (which was not changed).
	// The exact order depends on the initial priorities, but we check the priorities directly.

	providerMap := make(map[string]store.CloudProviderRecord)
	for _, p := range got {
		providerMap[p.Name] = p
	}

	// Verify "c" was not touched - it should still have its original priority.
	if providerMap["c"].Priority != cInitialPriority {
		t.Errorf("provider c: priority changed from %d to %d (should be untouched)",
			cInitialPriority, providerMap["c"].Priority)
	}

	// Verify "b" got priority 2 (first in the order slice, len=2 so 2-0=2).
	if providerMap["b"].Priority != 2 {
		t.Errorf("provider b: priority = %d, want 2", providerMap["b"].Priority)
	}

	// Verify "a" got priority 1 (second in the order slice, len=2 so 2-1=1).
	if providerMap["a"].Priority != 1 {
		t.Errorf("provider a: priority = %d, want 1", providerMap["a"].Priority)
	}
}

// TestOpenUpgradesPreCapRuntimeKeysSchema simulates an existing marbor.db from
// before the spend-cap columns existed: a runtime_keys table with none of
// daily_usd_cap/monthly_usd_cap. Open() must ALTER TABLE them in via the
// idempotent migration (sqlite.go migrate()) rather than erroring or leaving
// AddKey/AllKeys broken - this is the regression guard for a non-additive
// migration mistake going unnoticed (every other test uses a fresh DB where
// the ALTERs permanently no-op).
func TestOpenUpgradesPreCapRuntimeKeysSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "precap.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE runtime_keys (
		name          TEXT PRIMARY KEY,
		key           TEXT,
		rate_limit    INTEGER,
		daily_limit   INTEGER,
		monthly_limit INTEGER,
		models        TEXT,
		revoked       INTEGER
	)`); err != nil {
		t.Fatalf("create pre-cap runtime_keys: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on pre-cap schema: %v", err)
	}
	defer s.Close()

	k := store.KeyRecord{
		Name:         "upgraded-key",
		Key:          "sk-upgraded",
		RateLimit:    100,
		DailyLimit:   1000,
		MonthlyLimit: 10000,
		Models:       []string{"llama3"},
	}
	if err := s.UpsertKey(k); err != nil {
		t.Fatalf("UpsertKey after schema upgrade: %v", err)
	}

	keys, err := s.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys after schema upgrade: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].DailyUsdCap != 0 || keys[0].MonthlyUsdCap != 0 {
		t.Errorf("expected caps backfilled to 0, got daily=%v monthly=%v",
			keys[0].DailyUsdCap, keys[0].MonthlyUsdCap)
	}
}

// TestOpenMigratesModelConfigsToNodeKeyed simulates an existing marbor.db from
// before model_configs was keyed by (model, node): a model-only-PK table with
// one row. Open() must detect the old schema via migrateModelConfigsToNodeKeyed
// (sqlite.go), fan that row out to every known runtime_nodes entry, and
// preserve its original config fields - rather than erroring, dropping data,
// or leaving reads broken. This is the regression guard for the riskiest
// migration in migrate() (a real table rebuild + transaction), which every
// other test skips entirely because openTestDB/openTestDBAt always create the
// current (model, node)-keyed schema directly, so the rebuild path never runs.
func TestOpenMigratesModelConfigsToNodeKeyed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "premodelconfigs.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE model_configs (
		model       TEXT PRIMARY KEY,
		config_json TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create pre-node model_configs: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE runtime_nodes (
		name          TEXT PRIMARY KEY,
		url           TEXT,
		runtime       TEXT,
		vram_total_mb INTEGER
	)`); err != nil {
		t.Fatalf("create runtime_nodes: %v", err)
	}
	for _, name := range []string{"gpu-node-01", "gpu-node-02"} {
		if _, err := raw.Exec(`INSERT INTO runtime_nodes (name, url, runtime, vram_total_mb) VALUES (?, ?, ?, ?)`,
			name, "http://"+name+":11434", "ollama", 24000); err != nil {
			t.Fatalf("insert runtime_nodes %s: %v", name, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO model_configs (model, config_json) VALUES (?, ?)`,
		"the-model", `{"temperature":0.7}`); err != nil {
		t.Fatalf("insert pre-node model_configs row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on pre-node model_configs schema: %v", err)
	}
	defer s.Close()

	all, err := s.AllModelConfigs()
	if err != nil {
		t.Fatalf("AllModelConfigs after schema upgrade: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 fanned-out rows, got %d: %+v", len(all), all)
	}
	byNode := make(map[string]store.ModelConfig, len(all))
	for _, c := range all {
		if c.Model != "the-model" {
			t.Errorf("row model = %q, want the-model", c.Model)
		}
		if c.Temperature == nil || *c.Temperature != 0.7 {
			t.Errorf("row %s temperature = %v, want 0.7", c.Node, c.Temperature)
		}
		byNode[c.Node] = c
	}
	if _, ok := byNode["gpu-node-01"]; !ok {
		t.Error("missing fanned-out row for gpu-node-01")
	}
	if _, ok := byNode["gpu-node-02"]; !ok {
		t.Error("missing fanned-out row for gpu-node-02")
	}

	got, err := s.GetModelConfig("the-model", "gpu-node-01")
	if err != nil {
		t.Fatalf("GetModelConfig after schema upgrade: %v", err)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Fatalf("GetModelConfig temperature = %v, want 0.7", got.Temperature)
	}

	// The migration patches a "node" key into each fanned-out row's JSON blob
	// (sqlite.go migrateModelConfigsToNodeKeyed) - confirm it matches the row's
	// own node rather than being left stale/shared across rows.
	rows, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw for json check: %v", err)
	}
	defer rows.Close()
	r, err := rows.Query(`SELECT node, config_json FROM model_configs ORDER BY node`)
	if err != nil {
		t.Fatalf("query config_json: %v", err)
	}
	defer r.Close()
	for r.Next() {
		var node, cfgJSON string
		if err := r.Scan(&node, &cfgJSON); err != nil {
			t.Fatalf("scan config_json: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cfgJSON), &m); err != nil {
			t.Fatalf("unmarshal config_json for %s: %v", node, err)
		}
		var patchedNode string
		if err := json.Unmarshal(m["node"], &patchedNode); err != nil {
			t.Fatalf("unmarshal patched node field for %s: %v", node, err)
		}
		if patchedNode != node {
			t.Errorf("row %s: patched json node = %q, want %q", node, patchedNode, node)
		}
	}
}

// TestOpenUpgradesPreReasonNodeDrainSchema simulates an existing marbor.db from
// before node_drain had a drained_reason column. Open() must ALTER TABLE it
// in via the idempotent migration (sqlite.go migrate()) rather than erroring
// or leaving SetNodeDrain/NodeDrainStates broken. drained_reason is already
// part of the fresh CREATE TABLE node_drain, so - like the runtime_keys spend
// caps before this test existed - the ALTER for it permanently no-ops in
// every other test's fresh DB and this is the only regression guard against
// that migration silently breaking.
func TestOpenUpgradesPreReasonNodeDrainSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prereason.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE node_drain (
		name     TEXT PRIMARY KEY,
		draining INTEGER
	)`); err != nil {
		t.Fatalf("create pre-reason node_drain: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO node_drain (name, draining) VALUES (?, ?)`,
		"old-node", 1); err != nil {
		t.Fatalf("insert pre-reason node_drain row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open on pre-reason node_drain schema: %v", err)
	}
	defer s.Close()

	if err := s.SetNodeDrain("new-node", true, "maintenance"); err != nil {
		t.Fatalf("SetNodeDrain after schema upgrade: %v", err)
	}

	states, err := s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates after schema upgrade: %v", err)
	}
	if !states["old-node"].Draining {
		t.Error("old-node should still be draining after upgrade")
	}
	if states["old-node"].Reason != "" {
		t.Errorf("old-node reason = %q, want empty (backfilled default)", states["old-node"].Reason)
	}
	if !states["new-node"].Draining || states["new-node"].Reason != "maintenance" {
		t.Errorf("new-node = %+v, want draining=true reason=maintenance", states["new-node"])
	}
}

// TestBackupTo verifies BackupTo produces a valid, independently-openable
// SQLite database containing the same data as the live store, and that it
// refuses to overwrite an existing path (VACUUM INTO's own behavior).
func TestBackupTo(t *testing.T) {
	dir := t.TempDir()
	s := openTestDBAt(t, filepath.Join(dir, "live.db"))
	defer s.Close()

	if err := s.UpsertNode(store.NodeRecord{Name: "node-a", URL: "http://localhost:11434", Runtime: "ollama"}); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	if err := s.SetSetting("timezone", "UTC"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	backupPath := filepath.Join(dir, "backup.db")
	if err := s.BackupTo(backupPath); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	// The backup must be a fully independent, openable database with the
	// same data - not a reference to the live file.
	restored, err := store.Open(backupPath)
	if err != nil {
		t.Fatalf("Open(backup): %v", err)
	}
	defer restored.Close()

	nodes, err := restored.AllNodes()
	if err != nil {
		t.Fatalf("AllNodes on restored backup: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "node-a" {
		t.Fatalf("restored backup nodes = %+v, want [node-a]", nodes)
	}
	tz, err := restored.GetSetting("timezone")
	if err != nil || tz != "UTC" {
		t.Fatalf("restored backup timezone = %q, err=%v, want UTC", tz, err)
	}

	// VACUUM INTO refuses to write over an existing file - BackupTo must
	// surface that as an error, not silently succeed or corrupt the target.
	if err := s.BackupTo(backupPath); err == nil {
		t.Error("BackupTo to an already-existing path should return an error, got nil")
	}
}

// TestBenchmarkRunTPOTNullability verifies P408's new fields round-trip
// through InsertBenchmarkRun/ListBenchmarkRuns: p95/p99 and TPOT p50 are all
// nullable, preserving nil (not computable/not computed) vs. a real value
// (R1: absence, never a fabricated 0) across the SQL NULL boundary.
func TestBenchmarkRunTPOTNullability(t *testing.T) {
	s := openTestDB(t)

	tpot := 12.5
	coldP95, coldP99, warmP95, warmP99 := 1080.0, 1095.0, 108.0, 109.0
	withTPOT := store.BenchmarkRun{
		Node: "gpu-0", Model: "llama3:8b", N: 5,
		ColdP50Ms: 1000, ColdMinMs: 900, ColdMaxMs: 1100, ColdP95Ms: &coldP95, ColdP99Ms: &coldP99,
		WarmP50Ms: 100, WarmMinMs: 90, WarmMaxMs: 110, WarmP95Ms: &warmP95, WarmP99Ms: &warmP99,
		ColdTPOTP50Ms: &tpot, WarmTPOTP50Ms: nil,
		SpeedupX:  10,
		CreatedAt: time.Now(),
	}
	if err := s.InsertBenchmarkRun(withTPOT); err != nil {
		t.Fatalf("InsertBenchmarkRun (with TPOT): %v", err)
	}

	// noPercentiles simulates a row persisted before this migration added
	// p95/p99/TPOT (nil on every new field, exactly what a legacy row looks
	// like once ListBenchmarkRuns scans it) - it must come back with nil on
	// all four, never a backfilled/fabricated 0 (the bug this test guards).
	noPercentiles := store.BenchmarkRun{
		Node: "gpu-1", Model: "llama3:8b", N: 1,
		ColdP50Ms: 500, ColdMinMs: 500, ColdMaxMs: 500,
		WarmP50Ms: 50, WarmMinMs: 50, WarmMaxMs: 50,
		SpeedupX:  10,
		CreatedAt: time.Now(),
	}
	if err := s.InsertBenchmarkRun(noPercentiles); err != nil {
		t.Fatalf("InsertBenchmarkRun (no percentiles): %v", err)
	}

	runs, err := s.ListBenchmarkRuns(10)
	if err != nil {
		t.Fatalf("ListBenchmarkRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}

	var gotWithTPOT, gotNoPercentiles *store.BenchmarkRun
	for i := range runs {
		switch runs[i].Node {
		case "gpu-0":
			gotWithTPOT = &runs[i]
		case "gpu-1":
			gotNoPercentiles = &runs[i]
		}
	}
	if gotWithTPOT == nil || gotNoPercentiles == nil {
		t.Fatalf("missing expected rows: %+v", runs)
	}

	if gotWithTPOT.ColdP95Ms == nil || *gotWithTPOT.ColdP95Ms != 1080 || gotWithTPOT.ColdP99Ms == nil || *gotWithTPOT.ColdP99Ms != 1095 {
		t.Errorf("cold p95/p99 = %v/%v, want 1080/1095", gotWithTPOT.ColdP95Ms, gotWithTPOT.ColdP99Ms)
	}
	if gotWithTPOT.ColdTPOTP50Ms == nil || *gotWithTPOT.ColdTPOTP50Ms != 12.5 {
		t.Errorf("ColdTPOTP50Ms = %v, want 12.5", gotWithTPOT.ColdTPOTP50Ms)
	}
	if gotWithTPOT.WarmTPOTP50Ms != nil {
		t.Errorf("WarmTPOTP50Ms = %v, want nil", *gotWithTPOT.WarmTPOTP50Ms)
	}
	// The regression this test exists for: a row with no p95/p99 supplied
	// (standing in for a pre-P408 row after migration) must come back with
	// nil on ColdP95Ms/ColdP99Ms/WarmP95Ms/WarmP99Ms, not a fabricated 0.
	if gotNoPercentiles.ColdP95Ms != nil || gotNoPercentiles.ColdP99Ms != nil || gotNoPercentiles.WarmP95Ms != nil || gotNoPercentiles.WarmP99Ms != nil {
		t.Errorf("expected p95/p99 nil for a row with no percentile data, got cold=%v/%v warm=%v/%v",
			gotNoPercentiles.ColdP95Ms, gotNoPercentiles.ColdP99Ms, gotNoPercentiles.WarmP95Ms, gotNoPercentiles.WarmP99Ms)
	}
	if gotNoPercentiles.ColdTPOTP50Ms != nil || gotNoPercentiles.WarmTPOTP50Ms != nil {
		t.Errorf("expected both TPOT fields nil for a run with no computable TPOT, got cold=%v warm=%v", gotNoPercentiles.ColdTPOTP50Ms, gotNoPercentiles.WarmTPOTP50Ms)
	}
}
