package router

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func nodeLoadLockCount(r *Router) int {
	r.warmupInProgressMu.Lock()
	defer r.warmupInProgressMu.Unlock()
	return len(r.nodeLoadLocks)
}

func TestLockNodeLoad_PrunesIdleEntry(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	unlock := r.lockNodeLoad("n1")
	if got := nodeLoadLockCount(r); got != 1 {
		t.Fatalf("entries while held = %d, want 1", got)
	}
	unlock()
	if got := nodeLoadLockCount(r); got != 0 {
		t.Fatalf("entries after release = %d, want 0", got)
	}
}

func TestLockNodeLoad_ChurnedNamesDoNotAccumulate(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	for i := 0; i < 1000; i++ {
		r.lockNodeLoad("node-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String())()
	}
	if got := nodeLoadLockCount(r); got != 0 {
		t.Fatalf("entries after churn = %d, want 0", got)
	}
}

func TestLockNodeLoad_SameNameStillSerialized(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	var active, maxActive int32
	enter := func() {
		n := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&maxActive)
			if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
				break
			}
		}
	}

	unlockA := r.lockNodeLoad("n1")
	enter()
	bAcquired := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		unlockB := r.lockNodeLoad("n1")
		enter()
		close(bAcquired)
		atomic.AddInt32(&active, -1)
		unlockB()
	}()

	select {
	case <-bAcquired:
		t.Fatal("second holder acquired while first still held")
	case <-time.After(50 * time.Millisecond):
	}
	if got := nodeLoadLockCount(r); got != 1 {
		t.Fatalf("entries while held = %d, want 1", got)
	}
	atomic.AddInt32(&active, -1)
	unlockA()
	<-bDone
	if m := atomic.LoadInt32(&maxActive); m != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", m)
	}
	if got := nodeLoadLockCount(r); got != 0 {
		t.Fatalf("entries after both released = %d, want 0", got)
	}
}

// A node removed and re-added under the same name while a warmup holds the
// lock must still serialize against the next warmup: removal must not hand a
// new arrival a second mutex for the same name.
func TestLockNodeLoad_SurvivesNodeRemoveAndReAdd(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	node := config.NodeConfig{Name: "n1", URL: "http://10.0.0.1:11434"}
	if !r.AddNode(node) {
		t.Fatal("AddNode failed")
	}
	unlockA := r.lockNodeLoad("n1")
	r.RemoveNode("n1")
	if !r.AddNode(node) {
		t.Fatal("re-AddNode failed")
	}

	bAcquired := make(chan struct{})
	go func() {
		unlockB := r.lockNodeLoad("n1")
		close(bAcquired)
		unlockB()
	}()
	select {
	case <-bAcquired:
		t.Fatal("second lock acquired while first held across remove and re-add")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-bAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired after release")
	}
}
