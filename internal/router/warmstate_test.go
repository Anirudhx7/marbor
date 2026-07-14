package router

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func openWarmTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "warm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRestoreWarmStateSeedsResidencyAndLRU verifies that on startup the router
// re-seeds both the LRU last-used history and each node's residency map from the
// persisted warm state, but does NOT clobber residency a poll already populated.
func TestRestoreWarmStateSeedsResidencyAndLRU(t *testing.T) {
	st := openWarmTestStore(t)
	used := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := st.RecordWarmLoad(store.WarmStateRecord{Model: "llama3", Node: "cold", LastUsed: used, VRAMBytes: 4096}); err != nil {
		t.Fatalf("seed cold: %v", err)
	}
	if err := st.RecordWarmLoad(store.WarmStateRecord{Model: "mistral", Node: "polled", LastUsed: used, VRAMBytes: 8192}); err != nil {
		t.Fatalf("seed polled: %v", err)
	}

	r := &Router{
		nodes: []*NodeState{
			{Name: "cold", Healthy: true},
			// "polled" already has a live residency from a poll  --  must be preserved.
			{Name: "polled", Healthy: true, LoadedModels: []ModelInfo{{Name: "qwen", SizeVRAM: 1}}},
		},
	}
	r.SetStore(st)

	n, err := r.RestoreWarmState()
	if err != nil {
		t.Fatalf("RestoreWarmState: %v", err)
	}
	if n != 2 {
		t.Fatalf("restored %d pairs, want 2", n)
	}

	// The empty node is seeded from persisted state.
	if got := r.nodes[0].LoadedModels; len(got) != 1 || got[0].Name != "llama3" || got[0].SizeVRAM != 4096 {
		t.Errorf("cold node residency = %+v, want [llama3/4096]", got)
	}
	// The already-populated node keeps its live residency (poll wins).
	if got := r.nodes[1].LoadedModels; len(got) != 1 || got[0].Name != "qwen" {
		t.Errorf("polled node residency = %+v, want [qwen] (poll not clobbered)", got)
	}
	// LRU history restored for both pairs regardless of residency seeding.
	if got := r.lastUsedAt("cold", "llama3"); !got.Equal(used) {
		t.Errorf("cold llama3 last_used = %v, want %v", got, used)
	}
	if got := r.lastUsedAt("polled", "mistral"); !got.Equal(used) {
		t.Errorf("polled mistral last_used = %v, want %v", got, used)
	}
}

// TestFlushWarmStateSnapshotsResidency verifies the Tier-2/Tier-3 flush writes
// the current residency snapshot (with last-used) for every node.
func TestFlushWarmStateSnapshotsResidency(t *testing.T) {
	st := openWarmTestStore(t)
	r := &Router{
		nodes: []*NodeState{
			{Name: "n1", LoadedModels: []ModelInfo{{Name: "a", SizeVRAM: 100}, {Name: "b", SizeVRAM: 200}}},
			{Name: "n2", LoadedModels: []ModelInfo{{Name: "a", SizeVRAM: 300}}},
		},
	}
	r.SetStore(st)
	r.RecordModelUse("n1", "a")

	r.FlushWarmState()

	rows, err := st.AllWarmState()
	if err != nil {
		t.Fatalf("AllWarmState: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("flushed %d rows, want 3: %+v", len(rows), rows)
	}
	byKey := map[string]store.WarmStateRecord{}
	for _, w := range rows {
		byKey[w.Model+"@"+w.Node] = w
	}
	if w := byKey["a@n1"]; w.VRAMBytes != 100 || w.LastUsed.IsZero() {
		t.Errorf("a@n1 = %+v, want vram 100 and non-zero last_used", w)
	}
	if w := byKey["b@n1"]; w.VRAMBytes != 200 {
		t.Errorf("b@n1 vram = %d, want 200", w.VRAMBytes)
	}
	if w := byKey["a@n2"]; w.VRAMBytes != 300 {
		t.Errorf("a@n2 vram = %d, want 300", w.VRAMBytes)
	}
}

// TestPersistResidencyDiff verifies newly-resident models are recorded as loads
// and vanished models are deleted, immediately (Tier 1).
func TestPersistResidencyDiff(t *testing.T) {
	st := openWarmTestStore(t)
	r := &Router{}
	r.SetStore(st)

	prev := []ModelInfo{{Name: "a", SizeVRAM: 1}, {Name: "b", SizeVRAM: 2}}
	cur := []ModelInfo{{Name: "b", SizeVRAM: 2}, {Name: "c", SizeVRAM: 3}}
	r.persistResidencyDiff("n1", prev, cur)

	rows, _ := st.AllWarmState()
	got := map[string]bool{}
	for _, w := range rows {
		got[w.Model] = true
	}
	// "c" newly loaded -> recorded; "b" unchanged -> not written by diff; "a" gone.
	if !got["c"] {
		t.Errorf("newly-loaded c not recorded: %+v", rows)
	}
	if got["a"] {
		t.Errorf("vanished model a should have been deleted: %+v", rows)
	}
}

// TestWarmStateNilStoreIsNoop verifies a Router with no store never panics.
func TestWarmStateNilStoreIsNoop(t *testing.T) {
	r := &Router{nodes: []*NodeState{{Name: "n1", LoadedModels: []ModelInfo{{Name: "a"}}}}}
	r.FlushWarmState()
	r.persistResidencyDiff("n1", nil, []ModelInfo{{Name: "a"}})
	r.snapshotNode(r.nodes[0])
	if n, err := r.RestoreWarmState(); n != 0 || err != nil {
		t.Errorf("nil-store RestoreWarmState = (%d, %v), want (0, nil)", n, err)
	}
}

// TestWarmStateConcurrent exercises the warm-state paths under concurrency so the
// -race detector can prove there is no data race between the hot-path LRU stamp,
// the background flush, and restore.
func TestWarmStateConcurrent(t *testing.T) {
	st := openWarmTestStore(t)
	r := &Router{
		nodes: []*NodeState{
			{Name: "n1", LoadedModels: []ModelInfo{{Name: "a", SizeVRAM: 1}}},
			{Name: "n2", LoadedModels: []ModelInfo{{Name: "b", SizeVRAM: 2}}},
		},
	}
	r.SetStore(st)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch j % 4 {
				case 0:
					r.RecordModelUse("n1", "a")
				case 1:
					r.FlushWarmState()
				case 2:
					r.persistResidencyDiff("n2", nil, []ModelInfo{{Name: "b", SizeVRAM: 2}})
				case 3:
					_, _ = r.RestoreWarmState()
				}
			}
		}(i)
	}
	wg.Wait()
}
