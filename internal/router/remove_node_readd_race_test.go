package router

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestRemoveNodeSweepBlocksSameNameAddNode pins the node lifecycle
// serialization: RemoveNode's per-node-state sweep runs after r.mu is
// released, so without serialization a same-name AddNode could register the
// fresh node, have it write state, and then lose that state to the sweep
// still in flight for the removed node. The sweep is held mid-flight here by
// taking lruMu, which RemoveNode needs for its lastUsed sweep.
func TestRemoveNodeSweepBlocksSameNameAddNode(t *testing.T) {
	const nodeName = "gpu-1"
	const nodeURL = "http://10.0.0.5:11434"

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: nodeName, URL: nodeURL}}, nil)
	// AddNode below starts a one-shot poll of the (unreachable) node; remove
	// the node again at the end so nothing keeps targeting it.
	t.Cleanup(func() { r.RemoveNode(nodeName) })
	r.lruMu.Lock()
	r.lastUsed[modelKey(nodeName, "llama3")] = time.Now()

	removeDone := make(chan struct{})
	go func() {
		r.RemoveNode(nodeName)
		close(removeDone)
	}()

	// Wait until RemoveNode has dropped the node from the pool, i.e. it is
	// past the r.mu section and parked on lruMu in the sweep.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.RLock()
		n := len(r.nodes)
		r.mu.RUnlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			r.lruMu.Unlock()
			t.Fatal("RemoveNode never removed the node from the pool")
		}
		time.Sleep(time.Millisecond)
	}

	addDone := make(chan struct{})
	go func() {
		r.AddNode(config.NodeConfig{Name: nodeName, URL: nodeURL})
		close(addDone)
	}()

	select {
	case <-addDone:
		r.lruMu.Unlock()
		t.Fatal("same-name AddNode completed while RemoveNode's sweep was still in flight")
	case <-time.After(150 * time.Millisecond):
	}

	r.lruMu.Unlock()
	for _, ch := range []chan struct{}{removeDone, addDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("RemoveNode/AddNode did not finish after the sweep was released")
		}
	}
}
