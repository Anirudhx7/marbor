package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestEvictForHeadroomUsesLastKnownSizeWhenRuntimeReportsNone(t *testing.T) {
	const gib = int64(1) << 30
	r, _, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "no-size", SizeVRAM: 0}, // coldest; runtime reports no per-model size
		{Name: "B", SizeVRAM: 4 * gib},
	})
	r.RecordModelUse("n", "B")
	r.recordLastKnownVRAM("n", "no-size", 4*gib)

	needed := freeAfterOverhead(10240, 4*gib) + gib
	evicted := r.EvictForHeadroom(context.Background(), "n", "X", needed)
	got := rec.got()
	if evicted != 1 || len(got) != 1 || got[0] != "no-size" {
		t.Fatalf("evicted %d, unloaded %v; want the no-size model evicted using its last known 4 GiB", evicted, got)
	}
}

func TestEvictForHeadroomMultiModelRuntimeWithNoSizesIsEvictable(t *testing.T) {
	const gib = int64(1) << 30
	r, _, rec := evictFixture(t, 10240, []ModelInfo{
		{Name: "m1", SizeVRAM: 0},
		{Name: "m2", SizeVRAM: 0},
	})
	r.recordLastKnownVRAM("n", "m1", 6*gib)
	r.recordLastKnownVRAM("n", "m2", 6*gib)
	if evicted := r.EvictForHeadroom(context.Background(), "n", "X", 12*gib); evicted == 0 || len(rec.got()) == 0 {
		t.Fatalf("evicted %d, unloaded %v; models with a known last size must stay evictable", evicted, rec.got())
	}
}

func TestEnsureHeadroomPanicGivesCooldownClaimBack(t *testing.T) {
	r, n, _ := ensureHeadroomFixture(t, nil)
	r.store = panicDeleteStore{}
	func() {
		defer func() { _ = recover() }()
		r.ensureHeadroom(context.Background(), n, "X")
	}()
	r.evictMu.Lock()
	_, stamped := r.lastEvictAt["n"]
	r.evictMu.Unlock()
	if stamped {
		t.Fatal("a panic inside the eviction must not leave the cooldown claimed")
	}
}

func TestReleaseEvictClaimNeverClobbersNewerClaim(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	first, ok := r.claimEvictCooldown("n")
	if !ok {
		t.Fatal("first claim refused")
	}
	// Another caller takes over the claim with the very same timestamp.
	r.evictMu.Lock()
	stamp := r.lastEvictAt["n"]
	r.evictClaimSeq++
	r.evictClaimID["n"] = r.evictClaimSeq
	r.evictMu.Unlock()

	r.releaseEvictClaim("n", first)

	r.evictMu.Lock()
	got, still := r.lastEvictAt["n"]
	r.evictMu.Unlock()
	if !still || !got.Equal(stamp) {
		t.Fatal("releasing an older claim must not undo a newer one")
	}
}

func TestEstimateModelSizeCanonicalizesOverrideAndArchKey(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n", URL: "http://example.test:11434", Runtime: "vllm", VRAMOverrides: map[string]int64{"llama3": 5000}},
	}, nil)
	for _, name := range []string{"llama3", "llama3:latest"} {
		if got := r.estimateModelSizeBytes("http://example.test:11434", name, false, 0); got != 5000*1024*1024 {
			t.Errorf("estimate(%q) = %d, want the llama3 override", name, got)
		}
	}
	r.archFactsMu.Lock()
	r.modelArchFacts = map[string]ModelArchFacts{"llama3": {}}
	r.archFactsMu.Unlock()
	if _, ok := r.modelArchFactsFor("llama3:latest"); !ok {
		t.Error("arch facts recorded under the bare name must resolve for the :latest form")
	}
}

func TestLocalInterfaceAddrsBacksOffAfterFailure(t *testing.T) {
	prevFn := interfaceAddrs
	t.Cleanup(func() {
		interfaceAddrs = prevFn
		localAddrMu.Lock()
		localAddrCache = nil
		localAddrAt = time.Time{}
		localAddrRetryAt = time.Time{}
		localAddrMu.Unlock()
	})
	localAddrMu.Lock()
	localAddrCache = map[string]struct{}{"192.0.2.7": {}}
	localAddrAt = time.Now().Add(-2 * localAddrCacheTTL)
	localAddrRetryAt = time.Time{}
	localAddrMu.Unlock()
	calls := 0
	interfaceAddrs = func() ([]net.Addr, error) { calls++; return nil, errors.New("simulated failure") }

	for i := 0; i < 5; i++ {
		if _, ok := localInterfaceAddrs()["192.0.2.7"]; !ok {
			t.Fatal("previous set must keep being served")
		}
	}
	if calls != 1 {
		t.Fatalf("interface lookup ran %d times across 5 calls, want 1 (negative TTL)", calls)
	}
}

func TestUnloadModelViaAgentTruncatesAgentMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": strings.Repeat("e", 5000)})
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	r := New(config.RoutingConfig{}, nil, nil)
	err := r.unloadModelViaAgent(context.Background(), "http://"+host+":11434", MarborAgentConfig{Enabled: true, Port: port, Scheme: "http"}, "m")
	if err == nil {
		t.Fatal("want an error")
	}
	if len(err.Error()) > maxLogValueBytes+len("...") {
		t.Fatalf("error length %d, want agent message truncated", len(err.Error()))
	}
}

func TestHostLogKeysForNodesAndPanicsArePruned(t *testing.T) {
	now := time.Now()
	r := New(config.RoutingConfig{}, nil, nil)
	r.hostLogNow = func() time.Time { return now }
	keys := []string{"tags:n1", "probe:n1", "docker-add:http://x:1", "docker-discovery", "panic:h1", "panic:n1/m"}
	for _, k := range keys {
		if !r.allowHostLog(k) {
			t.Fatalf("first log for %q refused", k)
		}
	}
	now = now.Add(hostLogInterval / 2)
	r.dropHostEvidenceNotIn(map[string][]*NodeState{})
	r.hostEvidenceMu.RLock()
	young := len(r.hostLogAt)
	r.hostEvidenceMu.RUnlock()
	if young != len(keys) {
		t.Fatalf("young keys pruned early: %d left, want %d", young, len(keys))
	}

	now = now.Add(hostLogInterval)
	r.dropHostEvidenceNotIn(map[string][]*NodeState{})
	r.hostEvidenceMu.RLock()
	defer r.hostEvidenceMu.RUnlock()
	if len(r.hostLogAt) != 0 {
		t.Fatalf("expired keys not pruned: %v", r.hostLogAt)
	}
}

func TestRemoveNodeDropsItsLogKeys(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n1", URL: "http://example.test:11434"}}, nil)
	for _, k := range []string{"tags:n1", "probe:n1", "panic:n1", "panic:n1/m", "tags:other"} {
		r.allowHostLog(k)
	}
	r.RemoveNode("n1")
	r.hostEvidenceMu.RLock()
	defer r.hostEvidenceMu.RUnlock()
	if len(r.hostLogAt) != 1 {
		t.Fatalf("keys after RemoveNode = %v, want only tags:other", r.hostLogAt)
	}
}

func TestRecordModelUseChurnDoesNotDisplaceLoadedModelStamp(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "real"}}
	r.nodes[0].mu.Unlock()
	r.RecordModelUse("n", "real")
	for i := 0; i < 5*maxTrackedModelsPerNode; i++ {
		r.RecordModelUse("n", fmt.Sprintf("fake-%d", i))
	}
	if r.lastUsedAt("n", "real").IsZero() {
		t.Fatal("the stamp of a loaded model was pushed out by made-up model names")
	}
	prefix := modelKey("n", "")
	r.lruMu.Lock()
	count, tracked := 0, r.lastUsedCount["n"]
	for k := range r.lastUsed {
		if strings.HasPrefix(k, prefix) {
			count++
		}
	}
	r.lruMu.Unlock()
	if count > maxTrackedModelsPerNode || tracked != count {
		t.Fatalf("entries = %d, counter = %d, want equal and within the cap", count, tracked)
	}
}

func TestRecordLastKnownVRAMChurnKeepsResidentModel(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://example.test:11434"}}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "real"}}
	r.nodes[0].mu.Unlock()
	r.recordLastKnownVRAM("n", "real", 42)
	for i := 0; i < 5*maxTrackedModelsPerNode; i++ {
		r.recordLastKnownVRAM("n", fmt.Sprintf("fake-%d", i), 1)
	}
	if got := r.lastKnownVRAMBytes("n", "real"); got != 42 {
		t.Fatalf("size of the resident model = %d, want 42 kept", got)
	}
}

func TestRestoreWarmStateStaysWithinPerNodeCap(t *testing.T) {
	st := openWarmTestStore(t)
	base := time.Now().Add(-time.Hour)
	rows := maxTrackedModelsPerNode + 300
	for i := 0; i < rows; i++ {
		rec := store.WarmStateRecord{Model: fmt.Sprintf("m%d", i), Node: "cold", LastUsed: base.Add(time.Duration(i) * time.Second), VRAMBytes: 1}
		if err := st.RecordWarmLoad(rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r := &Router{nodes: []*NodeState{{Name: "cold", Healthy: true}}}
	r.SetStore(st)
	if _, err := r.RestoreWarmState(); err != nil {
		t.Fatalf("RestoreWarmState: %v", err)
	}
	r.lruMu.Lock()
	defer r.lruMu.Unlock()
	if len(r.lastUsed) > maxTrackedModelsPerNode {
		t.Fatalf("restored %d stamps, want at most the cap", len(r.lastUsed))
	}
}
