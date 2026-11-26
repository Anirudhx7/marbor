package router

import (
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

func newStallRouter(t *testing.T, upstreamMs int) (*Router, *NodeState) {
	t.Helper()
	r := New(config.RoutingConfig{
		Strategy:          "warm-first",
		PollIntervalMs:    2000,
		Fallback:          "least-connections",
		UpstreamTimeoutMs: upstreamMs,
	}, []config.NodeConfig{{Name: "a", URL: "http://a:11434"}}, nil)
	n := r.FindNode("a")
	if n == nil {
		t.Fatal("node a missing")
	}
	return r, n
}

func stallView(n *NodeState) (bool, int, string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	stalled, count, _, phase := n.StallViewLocked(time.Now())
	return stalled, count, phase
}

func TestStall_ThresholdNeedsThreeDistinctRequests(t *testing.T) {
	r, n := newStallRouter(t, 0)
	r.RecordStall("a", StallPhaseFirstByte, "req-1")
	r.RecordStall("a", StallPhaseFirstByte, "req-2")
	if r.IsStalled("a") {
		t.Fatal("two stalls must not flag the node")
	}
	r.RecordStall("a", StallPhaseFirstByte, "req-3")
	if !r.IsStalled("a") {
		t.Fatal("three distinct stalls must flag the node")
	}
	stalled, count, phase := stallView(n)
	if !stalled || count != 3 || phase != StallPhaseFirstByte {
		t.Fatalf("view = %v %d %q", stalled, count, phase)
	}
}

func TestStall_RepeatOfSameRequestCountsOnce(t *testing.T) {
	r, n := newStallRouter(t, 0)
	for i := 0; i < 5; i++ {
		r.RecordStall("a", StallPhaseFirstByte, "same")
	}
	if _, count, _ := stallView(n); count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestStall_EmptyRequestIDNeverCounted(t *testing.T) {
	r, n := newStallRouter(t, 0)
	r.RecordStall("a", StallPhaseFirstByte, "")
	if _, count, _ := stallView(n); count != 0 {
		t.Fatalf("count = %d, want 0 for an unidentified request", count)
	}
}

func TestStall_ClearResetsAndForgetsMarker(t *testing.T) {
	r, n := newStallRouter(t, 0)
	r.RecordStall("a", StallPhaseFirstByte, "req-1")
	r.RecordStall("a", StallPhaseFirstByte, "req-2")
	r.RecordStall("a", StallPhaseFirstByte, "req-3")
	r.ClearStalls("a")
	if r.IsStalled("a") {
		t.Fatal("clear must unflag the node")
	}
	stalled, count, phase := stallView(n)
	if stalled || count != 0 || phase != "" {
		t.Fatalf("after clear: %v %d %q", stalled, count, phase)
	}
	r.RecordStall("a", StallPhaseFirstByte, "req-3")
	if _, c, _ := stallView(n); c != 1 {
		t.Fatalf("a request seen before the clear counts again afterwards, got %d", c)
	}
}

func TestStall_ExpiresToUnknown(t *testing.T) {
	r, n := newStallRouter(t, 0)
	for _, id := range []string{"r1", "r2", "r3"} {
		r.RecordStall("a", StallPhaseFirstByte, id)
	}
	n.mu.Lock()
	n.LastStallAt = time.Now().Add(-StallExpiry - time.Second)
	n.mu.Unlock()
	if r.IsStalled("a") {
		t.Fatal("an expired record must read as unknown, not stalled")
	}
	n.mu.RLock()
	stalled, count, at, phase := n.StallViewLocked(time.Now())
	n.mu.RUnlock()
	if stalled || count != 0 || !at.IsZero() || phase != "" {
		t.Fatalf("expired view must be empty, got %v %d %v %q", stalled, count, at, phase)
	}
	// A new stall after expiry starts a fresh count.
	r.RecordStall("a", StallPhaseFirstByte, "r4")
	if _, c, _ := stallView(n); c != 1 {
		t.Fatalf("count after expiry = %d, want 1", c)
	}
}

func TestStall_UnknownNodeIsNoop(t *testing.T) {
	r, _ := newStallRouter(t, 0)
	r.RecordStall("nope", StallPhaseFirstByte, "x")
	r.ClearStalls("nope")
	if r.IsStalled("nope") {
		t.Fatal("unknown node is never stalled")
	}
}

func TestStall_ConcurrentRecordIsRaceFree(t *testing.T) {
	r, n := newStallRouter(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.RecordStall("a", StallPhaseFirstByte, string(rune('a'+i)))
			_ = r.IsStalled("a")
			stallView(n)
			if i%4 == 0 {
				r.ClearStalls("a")
			}
		}(i)
	}
	wg.Wait()
}

func TestStallDeadline_Formula(t *testing.T) {
	r, n := newStallRouter(t, 600000) // 10 min ceiling
	if got := r.FirstByteStallDeadline(n, 0); got != StallFirstByteFloor {
		t.Fatalf("no TTFT history: %v, want the floor %v", got, StallFirstByteFloor)
	}
	n.mu.Lock()
	n.RecentTTFT = []float64{2, 30, 5} // 4 x 30s = 120s beats the floor
	n.mu.Unlock()
	if got := r.FirstByteStallDeadline(n, 0); got != 120*time.Second {
		t.Fatalf("4x max TTFT: %v", got)
	}
	if got := r.FirstByteStallDeadline(n, 2); got != 360*time.Second {
		t.Fatalf("scaled by 1+2 others in flight: %v", got)
	}
	if got := r.FirstByteStallDeadline(n, 100); got != 10*time.Minute {
		t.Fatalf("never above the upstream timeout: %v", got)
	}
}

func TestStallDeadline_SmallUpstreamTimeoutIsTheCeiling(t *testing.T) {
	r, n := newStallRouter(t, 5000)
	if got := r.FirstByteStallDeadline(n, 0); got != 5*time.Second {
		t.Fatalf("got %v, want 5s", got)
	}
}
