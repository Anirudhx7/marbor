package router

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
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
