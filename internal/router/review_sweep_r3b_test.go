package router

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// A panic inside the headroom step must not leave the node load lock held: a
// later warm for the same node has to be able to take it.
func TestLoadWithHeadroomPanicDoesNotWedgeNodeLoadLock(t *testing.T) {
	r, n, _ := ensureHeadroomFixture(t, nil)
	r.store = panicDeleteStore{}
	func() {
		defer func() { _ = recover() }()
		_ = r.loadWithHeadroom(context.Background(), n, "X", "5m")
	}()
	got := make(chan struct{})
	go func() {
		unlock := r.lockNodeLoad("n")
		unlock()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the node load lock is still held after a panic in the headroom step")
	}
}

// Ollama reports size_vram, and 0 there can mean CPU-only, so a stale footprint
// from an earlier load must not be charged to it.
func TestEvictionDoesNotChargeStaleSizeToOllamaModel(t *testing.T) {
	r, _, rec := evictFixture(t, 10240, []ModelInfo{{Name: "cpu-only", SizeVRAM: 0}})
	r.recordLastKnownVRAM("n", "cpu-only", 8*gibBytes)
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", 5*gibBytes); evicted != 0 || len(rec.got()) != 0 {
		t.Fatalf("evicted %d, unloaded %v; a zero-size Ollama model uses no counted VRAM", evicted, rec.got())
	}
}

// Eviction and ensureHeadroom must count a loaded model the same way.
func TestEffectiveModelBytesAgreesAcrossCallers(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.recordLastKnownVRAM("n", "m", 7*gibBytes)
	cases := []struct {
		runtime  string
		reported int64
		want     int64
	}{
		{"ollama", 0, 0},
		{"ollama", 2 * gibBytes, 2 * gibBytes},
		{"vllm", 0, 7 * gibBytes},
		{"", 0, 7 * gibBytes},
		{"vllm", 3 * gibBytes, 3 * gibBytes},
	}
	for _, c := range cases {
		if got := r.effectiveModelBytes("n", c.runtime, c.reported, "m"); got != c.want {
			t.Errorf("runtime %q reported %d: got %d want %d", c.runtime, c.reported, got, c.want)
		}
	}
	// ensureHeadroom on a runtime with no per-model sizes must see the node as
	// full from the last-known sizes, like eviction does, and so try to evict.
	rr, n, rec := evictFixture(t, 10240, []ModelInfo{{Name: "a"}, {Name: "b"}})
	setNodeRuntime(rr, "")
	rr.recordLastKnownVRAM("n", "a", 4*gibBytes)
	rr.recordLastKnownVRAM("n", "b", 4*gibBytes)
	n.mu.Lock()
	n.VRAMOverrides = map[string]int64{"X": 6000}
	n.mu.Unlock()
	_ = rr.makeHeadroom(context.Background(), n, "X")
	if len(rec.got()) == 0 {
		t.Fatal("ensureHeadroom did not evict although the node is full by last-known sizes")
	}
}

// Credit equals charge, and eviction stops as soon as enough is free.
func TestEvictForHeadroomStopsOnceEnoughIsFreed(t *testing.T) {
	setup := func() (*Router, *unloadRecorder) {
		r, _, rec := evictFixture(t, 12288, []ModelInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}})
		setNodeRuntime(r, "")
		for _, m := range []string{"a", "b", "c"} {
			r.recordLastKnownVRAM("n", m, 3*gibBytes)
		}
		return r, rec
	}
	// Used 9 GiB x overhead leaves 1.2 GiB free; each eviction credits
	// 3 GiB x the same overhead (3.6).
	r, rec := setup()
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", 4*gibBytes); evicted != 1 || len(rec.got()) != 1 {
		t.Fatalf("need 4 GiB: evicted %d (%v), want exactly 1", evicted, rec.got())
	}
	r, rec = setup()
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", 8*gibBytes); evicted != 2 || len(rec.got()) != 2 {
		t.Fatalf("need 8 GiB: evicted %d (%v), want exactly 2", evicted, rec.got())
	}
}

func TestUndoIfNodeRemovedOnlyActsForMissingNodes(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "live", URL: "http://example.test:11434"}}, nil)
	r.RecordModelUse("live", "m")
	r.recordLastKnownVRAM("live", "m", 5)
	// A write that landed for a node that is already gone.
	r.lruMu.Lock()
	r.stampLastUsedLocked("gone", "m", time.Now(), nil)
	r.lruMu.Unlock()
	r.vramSeenMu.Lock()
	r.storeLastKnownLocked("gone", modelKey("gone", "m"), 9, nil)
	r.vramSeenMu.Unlock()

	r.undoIfNodeRemoved("gone", r.forgetLastUsedForNode)
	r.undoIfNodeRemoved("gone", r.forgetLastKnownForNode)
	r.undoIfNodeRemoved("live", r.forgetLastUsedForNode)
	r.undoIfNodeRemoved("live", r.forgetLastKnownForNode)

	if !r.lastUsedAt("gone", "m").IsZero() || r.lastKnownVRAMBytes("gone", "m") != 0 {
		t.Fatal("bookkeeping for a removed node was not undone")
	}
	r.lruMu.Lock()
	_, idx := r.lastUsedKeys["gone"]
	_, cnt := r.lastUsedCount["gone"]
	r.lruMu.Unlock()
	if idx || cnt {
		t.Fatal("the removed node key index or counter survived the undo")
	}
	if r.lastUsedAt("live", "m").IsZero() || r.lastKnownVRAMBytes("live", "m") != 5 {
		t.Fatal("the undo touched a node that still exists")
	}
}

// The re-check and the undo run under the node lifecycle lock, so a same-name
// AddNode cannot interleave with them.
func TestUndoIfNodeRemovedHoldsLifecycleLock(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	r.nodeLifecycleMu.Lock()
	done := make(chan struct{})
	go func() {
		r.undoIfNodeRemoved("gone", func(string) {})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the undo ran while another node lifecycle change was in progress")
	case <-time.After(150 * time.Millisecond):
	}
	r.nodeLifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the undo never ran after the lifecycle lock was released")
	}
}

func TestRecordModelUseGathersLoadedSetWhenFullThenStamps(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "resident"}}
	r.nodes[0].mu.Unlock()
	old := time.Now().Add(-3 * time.Hour)
	r.lruMu.Lock()
	r.stampLastUsedLocked("n", "resident", old, nil)
	for i := 1; i < maxTrackedModelsPerNode; i++ {
		r.stampLastUsedLocked("n", fmt.Sprintf("fill-%d", i), time.Now().Add(-2*time.Hour), nil)
	}
	r.lruMu.Unlock()
	r.RecordModelUse("n", "brand-new")
	if r.lastUsedAt("n", "brand-new").IsZero() {
		t.Fatal("the new model was not stamped after the node was trimmed")
	}
	if r.lastUsedAt("n", "resident").IsZero() {
		t.Fatal("the loaded model lost its stamp to the trim")
	}
	r.lruMu.Lock()
	count := r.lastUsedCount["n"]
	keys := len(r.lastUsedKeys["n"])
	r.lruMu.Unlock()
	if count > maxTrackedModelsPerNode || keys > maxTrackedModelsPerNode {
		t.Fatalf("count %d keys %d exceed the cap", count, keys)
	}
}

// Trim order: neither loaded nor recent goes first, then recent, loaded last.
func TestTrimNodeEntriesKeepTiers(t *testing.T) {
	now := time.Now()
	m := map[string]time.Time{}
	keys := map[string]struct{}{}
	add := func(name string, at time.Time) {
		k := modelKey("n", name)
		m[k] = at
		keys[k] = struct{}{}
	}
	for i := 0; i < 100; i++ {
		add(fmt.Sprintf("old-%d", i), now.Add(-2*time.Hour))
	}
	for i := 0; i < 99; i++ {
		add(fmt.Sprintf("recent-%d", i), now.Add(-time.Duration(i)*time.Millisecond))
	}
	add("loaded", now.Add(-5*time.Hour))
	loaded := map[string]bool{canonicalModelName("loaded"): true}
	remaining := trimNodeEntries(m, keys, "n", loaded, now.Add(-time.Minute), func(t time.Time) time.Time { return t })
	if remaining != 200-trimBatch {
		t.Fatalf("remaining %d, want %d", remaining, 200-trimBatch)
	}
	if _, ok := m[modelKey("n", "loaded")]; !ok {
		t.Fatal("a loaded model was dropped before unloaded ones")
	}
	for i := 0; i < 100; i++ {
		if _, ok := m[modelKey("n", fmt.Sprintf("old-%d", i))]; ok {
			t.Fatalf("old-%d survived while recent entries were also at risk", i)
		}
	}
	if len(keys) != len(m) {
		t.Fatalf("key index (%d) and map (%d) disagree after a trim", len(keys), len(m))
	}
}

func TestTrimNodeEntriesTieBreakIsDeterministic(t *testing.T) {
	build := func() (map[string]time.Time, map[string]struct{}) {
		at := time.Now().Add(-time.Hour)
		m, keys := map[string]time.Time{}, map[string]struct{}{}
		for i := 0; i < 300; i++ {
			k := modelKey("n", fmt.Sprintf("m-%03d", i))
			m[k], keys[k] = at, struct{}{}
		}
		return m, keys
	}
	var first map[string]time.Time
	for run := 0; run < 5; run++ {
		m, keys := build()
		trimNodeEntries(m, keys, "n", nil, time.Time{}, func(t time.Time) time.Time { return t })
		if first == nil {
			first = m
			continue
		}
		for k := range first {
			if _, ok := m[k]; !ok {
				t.Fatalf("run %d dropped a different set of equal-stamp entries", run)
			}
		}
		if len(m) != len(first) {
			t.Fatalf("run %d kept %d entries, first kept %d", run, len(m), len(first))
		}
	}
	if _, ok := first[modelKey("n", "m-000")]; ok {
		t.Fatal("equal stamps must drop the lowest names first")
	}
}

// Rotating names on one node must not touch another node entries.
func TestTrimIsPerNode(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "a", URL: "http://example.test:1"}, {Name: "b", URL: "http://example.test:2"},
	}, nil)
	for i := 0; i < 50; i++ {
		r.RecordModelUse("b", fmt.Sprintf("b-%d", i))
	}
	for i := 0; i < 3*maxTrackedModelsPerNode; i++ {
		r.RecordModelUse("a", fmt.Sprintf("a-%d", i))
	}
	for i := 0; i < 50; i++ {
		if r.lastUsedAt("b", fmt.Sprintf("b-%d", i)).IsZero() {
			t.Fatalf("trimming node a dropped b-%d", i)
		}
	}
}

// Recording the sizes for one poll must not take the router lock per model.
func TestPutLastKnownVRAMDoesNotNeedRouterLock(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.mu.Lock()
	done := make(chan struct{})
	go func() {
		r.putLastKnownVRAM("n", "m", 5)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.mu.Unlock()
		t.Fatal("putLastKnownVRAM blocked on the router lock")
	}
	r.mu.Unlock()
	if r.lastKnownVRAMBytes("n", "m") != 5 {
		t.Fatal("size not recorded")
	}
}

func TestWarmReservationsAreBoundedAndExpiredOnesDeleted(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	for i := 0; i < 3*maxWarmReservationsPerNode; i++ {
		r.reserveWarmBytes("n", fmt.Sprintf("made-up-%d", i), 1)
	}
	r.evictMu.Lock()
	size := len(r.warmReserved["n"])
	r.evictMu.Unlock()
	if size > maxWarmReservationsPerNode {
		t.Fatalf("%d reservations held, want at most %d", size, maxWarmReservationsPerNode)
	}
	// Expired entries are deleted when read, not only skipped.
	r.evictMu.Lock()
	for m, res := range r.warmReserved["n"] {
		res.at = time.Now().Add(-2 * warmReservationTTL)
		r.warmReserved["n"][m] = res
	}
	r.evictMu.Unlock()
	if got := r.PendingPrewarmBytes("n"); got != 0 {
		t.Fatalf("expired reservations still counted: %d", got)
	}
	r.evictMu.Lock()
	left := len(r.warmReserved["n"])
	r.evictMu.Unlock()
	if left != 0 {
		t.Fatalf("%d expired reservations were left in the map", left)
	}
}

func TestMakeHeadroomDistinguishesCooldownFromNoRoom(t *testing.T) {
	r, n, _ := ensureHeadroomFixture(t, nil)
	if _, ok := r.claimEvictCooldown("n"); !ok {
		t.Fatal("setup: could not claim the cooldown")
	}
	if err := r.makeHeadroom(context.Background(), n, "X"); err != errHeadroomCooldown {
		t.Fatalf("with the cooldown running got %v, want errHeadroomCooldown", err)
	}
	r2, n2, _ := ensureHeadroomFixture(t, nil)
	r2.SetPinnedModels("n", []string{"A", "B"})
	if err := r2.makeHeadroom(context.Background(), n2, "X"); err != errNoHeadroom {
		t.Fatalf("with everything pinned got %v, want errNoHeadroom", err)
	}
	if errHeadroomCooldown.Error() == errNoHeadroom.Error() {
		t.Fatal("the two outcomes must read differently")
	}
}
