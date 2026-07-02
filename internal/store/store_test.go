package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
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

	if err := s.SetNodeDrain("node1", true); err != nil {
		t.Fatalf("SetNodeDrain: %v", err)
	}
	if err := s.SetNodeDrain("node2", false); err != nil {
		t.Fatalf("SetNodeDrain: %v", err)
	}

	states, err := s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates: %v", err)
	}
	if !states["node1"] {
		t.Error("node1 should be draining")
	}
	if states["node2"] {
		t.Error("node2 should not be draining")
	}

	// Update node1 to not draining.
	if err := s.SetNodeDrain("node1", false); err != nil {
		t.Fatalf("SetNodeDrain update: %v", err)
	}
	states, err = s.NodeDrainStates()
	if err != nil {
		t.Fatalf("NodeDrainStates after update: %v", err)
	}
	if states["node1"] {
		t.Error("node1 should no longer be draining")
	}
}

// TestNodeOverrides verifies UpsertNodeOverride + NodeOverrides.
func TestNodeOverrides(t *testing.T) {
	s := openTestDB(t)

	vram := int64(16384)
	gpu := "NVIDIA RTX 4090"

	if err := s.UpsertNodeOverride("node1", &vram, &gpu); err != nil {
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
