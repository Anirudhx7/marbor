package router

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func newViewTestNode() *NodeState {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://localhost:1"}}, nil)
	return r.nodes[0]
}

// The view builder must hand back exactly what the self-locking accessors
// return, for every combination of declared and detected state.
func TestPlacementRequirementView_MatchesAccessors(t *testing.T) {
	verified := func() *marboragent.GPUScope {
		return &marboragent.GPUScope{Indices: []int{0, 1}, UUIDs: []string{"u0", "u1"}, CrossChecked: true}
	}
	unverified := func() *marboragent.GPUScope {
		return &marboragent.GPUScope{Indices: []int{0, 1}}
	}
	cases := []struct {
		name  string
		setup func(n *NodeState)
	}{
		{"nothing set", func(n *NodeState) {}},
		{"declared only", func(n *NodeState) { n.ParallelismType = "tp"; n.ParallelismWidth = 4 }},
		{"declared indices only", func(n *NodeState) { n.DeclaredGPUIndices = []int{0, 1, 2} }},
		{"detected unverified", func(n *NodeState) {
			n.DetectedParallelismType = "tp"
			n.DetectedParallelismWidth = 2
			n.DetectedGPUGroup = []int{0, 1}
			n.DetectedGPUScope = unverified()
		}},
		{"detected verified", func(n *NodeState) {
			n.DetectedParallelismType = "tp"
			n.DetectedParallelismWidth = 2
			n.DetectedGPUGroup = []int{0, 1}
			n.DetectedPipelineWidth = 2
			n.DetectedDataWidth = 3
			n.DetectedGPUScope = verified()
		}},
		{"detected verified but nil scope", func(n *NodeState) {
			n.DetectedParallelismWidth = 2
			n.DetectedGPUScope = nil
		}},
		{"declared vs detected width mismatch", func(n *NodeState) {
			n.ParallelismType = "tp"
			n.ParallelismWidth = 4
			n.DetectedParallelismType = "tp"
			n.DetectedParallelismWidth = 8
			n.DetectedGPUScope = verified()
		}},
		{"type mismatch at equal width", func(n *NodeState) {
			n.ParallelismType = "tp"
			n.ParallelismWidth = 4
			n.DetectedParallelismType = "pp"
			n.DetectedParallelismWidth = 4
			n.DetectedGPUScope = verified()
		}},
		{"declared wins over verified detected", func(n *NodeState) {
			n.ParallelismWidth = 2
			n.DetectedParallelismWidth = 2
			n.DetectedGPUScope = verified()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newViewTestNode()
			n.Lock()
			tc.setup(n)
			n.Unlock()

			n.RLock()
			v := n.PlacementRequirementViewLocked()
			n.RUnlock()

			if want := n.EffectiveRequiredGPUs(); v.EffectiveRequiredGPUs != want {
				t.Errorf("EffectiveRequiredGPUs = %d, want %d", v.EffectiveRequiredGPUs, want)
			}
			if want := n.EffectiveDetectedRequiredGPUs(); v.DetectedEffectiveRequiredGPUs != want {
				t.Errorf("DetectedEffectiveRequiredGPUs = %d, want %d", v.DetectedEffectiveRequiredGPUs, want)
			}
			if want := n.MismatchWarning(); v.MismatchWarning != want {
				t.Errorf("MismatchWarning = %q, want %q", v.MismatchWarning, want)
			}
			p, d := n.DetectedSecondaryWidths()
			if v.PipelineWidth != p || v.DataWidth != d {
				t.Errorf("widths = %d/%d, want %d/%d", v.PipelineWidth, v.DataWidth, p, d)
			}
			if want := n.DetectedDrivesPlacement(); v.DrivesPlacement != want {
				t.Errorf("DrivesPlacement = %v, want %v", v.DrivesPlacement, want)
			}
			want := n.DetectedGPUScopeCopy()
			if (v.GPUScope == nil) != (want == nil) {
				t.Fatalf("GPUScope nil = %v, want nil = %v", v.GPUScope == nil, want == nil)
			}
			if want != nil && (v.GPUScope.CrossChecked != want.CrossChecked || len(v.GPUScope.Indices) != len(want.Indices)) {
				t.Errorf("GPUScope = %+v, want %+v", v.GPUScope, want)
			}
		})
	}
}

// The view's scope is a deep copy: the response is encoded after the lock is
// released, so it must not alias the node's own slices.
func TestPlacementRequirementView_ScopeIsDeepCopy(t *testing.T) {
	n := newViewTestNode()
	n.Lock()
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, UUIDs: []string{"a", "b"}, CrossChecked: true}
	n.Unlock()

	n.RLock()
	v := n.PlacementRequirementViewLocked()
	n.RUnlock()
	v.GPUScope.Indices[0] = 99
	v.GPUScope.UUIDs[0] = "changed"
	v.GPUScope.CrossChecked = false

	n.RLock()
	defer n.RUnlock()
	if n.DetectedGPUScope.Indices[0] != 0 || n.DetectedGPUScope.UUIDs[0] != "a" || !n.DetectedGPUScope.CrossChecked {
		t.Errorf("node scope was changed through the view: %+v", n.DetectedGPUScope)
	}
}

// Building the view must not take the node lock again. With the read lock
// held and a writer queued behind it, TryRLock failing proves the writer is
// waiting, so any nested RLock would block forever.
func TestPlacementRequirementView_NoNestedLock(t *testing.T) {
	n := newViewTestNode()
	n.Lock()
	n.DetectedParallelismWidth = 2
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, CrossChecked: true}
	n.Unlock()

	n.RLock()
	writerDone := make(chan struct{})
	go func() {
		n.Lock()
		n.Unlock()
		close(writerDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for n.mu.TryRLock() {
		n.mu.RUnlock()
		if time.Now().After(deadline) {
			n.RUnlock()
			t.Fatal("writer never queued")
		}
		time.Sleep(time.Millisecond)
	}

	done := make(chan PlacementRequirementView, 1)
	go func() { done <- n.PlacementRequirementViewLocked() }()
	select {
	case v := <-done:
		if v.DetectedEffectiveRequiredGPUs != 2 || !v.DrivesPlacement {
			t.Errorf("unexpected view %+v", v)
		}
	case <-time.After(2 * time.Second):
		n.RUnlock()
		t.Fatal("PlacementRequirementViewLocked blocked behind a queued writer")
	}
	n.RUnlock()
	<-writerDone
}

// Guard against the nested-read-lock mistake coming back. This is a plain
// text scan, so it only catches direct calls to the self-locking accessors
// inside the named functions, not calls reached through other helpers.
func TestNoSelfLockingAccessorsUnderNodeLock(t *testing.T) {
	selfLocking := []string{
		".EffectiveRequiredGPUs(", ".EffectiveDetectedRequiredGPUs(", ".EffectiveDeclaredRequiredGPUs(",
		".MismatchWarning(", ".DetectedDrivesPlacement(", ".DetectedGPUScopeCopy(", ".DetectedSecondaryWidths(",
	}
	targets := []struct{ file, marker string }{
		{"../admin/admin.go", "func (s *Server) nodeStateToResp("},
		{"../admin/node_detected.go", "func newNodeDetectedExtras("},
		{"router.go", "func (n *NodeState) PlacementRequirementViewLocked("},
	}
	for _, tg := range targets {
		src, err := os.ReadFile(tg.file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		start := strings.Index(text, tg.marker)
		if start < 0 {
			t.Fatalf("%s: %q not found", tg.file, tg.marker)
		}
		body := text[start:]
		if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
			body = body[:end+1]
		}
		for _, call := range selfLocking {
			if strings.Contains(body, call) {
				t.Errorf("%s: %s calls %s while the caller holds the node lock", tg.file, tg.marker, call)
			}
		}
	}
}
