package router

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func assertDrainCarried(t *testing.T, r *Router, name string) {
	t.Helper()
	r.mu.RLock()
	var n *NodeState
	for _, c := range r.nodes {
		if c.Name == name {
			n = c
		}
	}
	r.mu.RUnlock()
	if n == nil {
		t.Fatalf("node %q missing", name)
	}
	n.mu.RLock()
	draining, reason, grace, prewarm := n.Draining, n.DrainedReason, n.DrainedGraceSeconds, n.PrewarmDisabled
	n.mu.RUnlock()
	if !draining || reason != "manual" || grace != 30 || !prewarm {
		t.Errorf("drain state lost: draining=%v reason=%q grace=%d prewarmDisabled=%v", draining, reason, grace, prewarm)
	}
	if got, _, _ := r.Route("", "", ""); got != nil && got.Name == name {
		t.Error("a drained node must stay excluded from routing after its URL changes")
	}
}

func TestUpdateNodeURL_KeepsDrainState(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	r.DrainNode("gpu-0", "manual", 30)
	r.SetPrewarmDisabled("gpu-0", true)
	if err := r.UpdateNodeURL("gpu-0", "http://127.0.0.1:2"); err != nil {
		t.Fatalf("UpdateNodeURL: %v", err)
	}
	assertDrainCarried(t, r, "gpu-0")
}

func TestAddNode_URLChangeKeepsDrainState(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	r.DrainNode("gpu-0", "manual", 30)
	r.SetPrewarmDisabled("gpu-0", true)
	if !r.AddNode(config.NodeConfig{Name: "gpu-0", URL: "http://127.0.0.1:2"}) {
		t.Fatal("AddNode upsert returned false")
	}
	assertDrainCarried(t, r, "gpu-0")
}

// Hot-path readers (the proxy parses node.URL after Route returns) read the
// URL without taking the node lock, so a URL change must publish a new
// NodeState instead of rewriting the live one.
func TestAddNode_URLChangeDoesNotMutateLiveNodeState(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n, _, _ := r.Route("", "", ""); n != nil {
					_ = n.URL // lock-free read, as the proxy does
					_ = n.Host
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.AddNode(config.NodeConfig{Name: "gpu-0", URL: fmt.Sprintf("http://127.0.0.1:%d", 2+i%2)})
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

func urlChangeBookkeeping(t *testing.T, change func(r *Router)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "url-change.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	oldURL := "http://127.0.0.1:1"
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: oldURL}}, nil)
	r.store = st
	if err := st.RecordWarmLoad(store.WarmStateRecord{Model: "m", Node: "gpu-0"}); err != nil {
		t.Fatalf("RecordWarmLoad: %v", err)
	}
	r.discoveredURLs[oldURL] = struct{}{}
	r.tagsMu.Lock()
	r.tagsCache[oldURL] = &TagsCache{FetchedAt: time.Now()}
	r.tagsMu.Unlock()

	change(r)

	if _, ok := r.discoveredURLs[oldURL]; ok {
		t.Error("old URL must be dropped from the discovered set")
	}
	r.tagsMu.RLock()
	_, cached := r.tagsCache[oldURL]
	r.tagsMu.RUnlock()
	if cached {
		t.Error("old URL's model-tag cache must be dropped")
	}
	rows, err := st.AllWarmState()
	if err != nil {
		t.Fatalf("AllWarmState: %v", err)
	}
	for _, w := range rows {
		if w.Node == "gpu-0" {
			t.Errorf("persisted warm state for the node must be cleared on a URL change: %+v", w)
		}
	}
}

func TestUpdateNodeURL_ClearsURLBookkeeping(t *testing.T) {
	urlChangeBookkeeping(t, func(r *Router) {
		if err := r.UpdateNodeURL("gpu-0", "http://127.0.0.1:2"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAddNode_URLChangeClearsURLBookkeeping(t *testing.T) {
	urlChangeBookkeeping(t, func(r *Router) {
		if !r.AddNode(config.NodeConfig{Name: "gpu-0", URL: "http://127.0.0.1:2"}) {
			t.Fatal("AddNode returned false")
		}
	})
}

// An AddNode with a new URL and no declared VRAM must not zero the node's
// known VRAM total, or headroom math would see a node with no memory.
func TestAddNode_URLChangeKeepsVRAMTotals(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", VRAMTotalMB: 4096},
	}, nil)
	r.nodes[0].VRAMSource = "agent"
	r.nodes[0].VRAMTotalMB = 8192
	r.AddNode(config.NodeConfig{Name: "gpu-0", URL: "http://127.0.0.1:2"})
	r.mu.RLock()
	n := r.nodes[0]
	r.mu.RUnlock()
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.VRAMTotalMB != 8192 || n.VRAMSource != "agent" || n.VRAMTotalMBConfig != 4096 {
		t.Errorf("vram total=%d source=%q config=%d, want 8192/agent/4096", n.VRAMTotalMB, n.VRAMSource, n.VRAMTotalMBConfig)
	}
}

// Re-submitting an unchanged node must not write fields that request
// handlers read without the node lock.
func TestAddNode_SameURLUpsertRaceFreeWithReaders(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1", Runtime: "ollama"},
	}, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n, _, _ := r.Route("", "", ""); n != nil {
					_ = n.URL
					_ = n.Host
					_ = n.Runtime
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.AddNode(config.NodeConfig{Name: "gpu-0", URL: "http://127.0.0.1:1", Runtime: "ollama"})
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
}
