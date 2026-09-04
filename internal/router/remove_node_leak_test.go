package router

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestRemoveNodePurgesWarmupSuppressedAndShowCache is a regression test for
// B1 finding ROUTER-05: RemoveNode used to leave warmupSuppressed (keyed by
// node name) and showCache (keyed by "nodeURL|tag") entries in place for a
// removed node - a ghost warmupSuppressed entry would forever report the
// removed node as suppressed if its name were ever reused, and showCache
// accumulated dead entries for the removed node's URL until their own TTL/
// size-cap eviction.
func TestRemoveNodePurgesWarmupSuppressedAndShowCache(t *testing.T) {
	const nodeName = "gpu-1"
	const nodeURL = "http://10.0.0.5:11434"

	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: nodeName, URL: nodeURL}}, nil)

	r.suppressWarmup(nodeName, "llama3", "manual")
	if !r.isWarmupSuppressed(nodeName, "llama3") {
		t.Fatal("setup: expected warmup suppressed before RemoveNode")
	}

	r.showMu.Lock()
	r.showCache[nodeURL+"|latest"] = modelShowCacheEntry{OK: true, FetchedAt: time.Now()}
	r.showMu.Unlock()

	r.RemoveNode(nodeName)

	if r.isWarmupSuppressed(nodeName, "llama3") {
		t.Error("warmupSuppressed entry for removed node was not purged by RemoveNode")
	}

	r.showMu.Lock()
	_, stillCached := r.showCache[nodeURL+"|latest"]
	r.showMu.Unlock()
	if stillCached {
		t.Error("showCache entry for removed node's URL was not purged by RemoveNode")
	}
}
