package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
			n.DetectedPipelineWidth = 5
			n.DetectedDataWidth = 7
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
		{"detected width, nil scope (unverified)", func(n *NodeState) {
			n.DetectedParallelismWidth = 2
			n.DetectedGPUScope = nil
		}},
		{"declared vs detected width mismatch", func(n *NodeState) {
			n.ParallelismType = "tp"
			n.ParallelismWidth = 4
			n.DetectedParallelismType = "tp"
			n.DetectedParallelismWidth = 8
			n.DetectedPipelineWidth = 3
			n.DetectedDataWidth = 2
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
			if !reflect.DeepEqual(v.GPUScope, want) {
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
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, UUIDs: []string{"a", "b"}, Source: "src", Note: "note", CrossChecked: true}
	n.Unlock()

	n.RLock()
	v := n.PlacementRequirementViewLocked()
	n.RUnlock()
	v.GPUScope.Indices[0] = 99
	v.GPUScope.UUIDs[0] = "changed"
	v.GPUScope.Source = "other"
	v.GPUScope.Note = "other"
	v.GPUScope.CrossChecked = false

	n.RLock()
	defer n.RUnlock()
	if n.DetectedGPUScope.Indices[0] != 0 || n.DetectedGPUScope.UUIDs[0] != "a" || !n.DetectedGPUScope.CrossChecked ||
		n.DetectedGPUScope.Source != "src" || n.DetectedGPUScope.Note != "note" {
		t.Errorf("node scope was changed through the view: %+v", n.DetectedGPUScope)
	}
}

// A node with no scope yields a nil view scope.
func TestPlacementRequirementView_NilScopeStaysNil(t *testing.T) {
	n := newViewTestNode()
	n.RLock()
	v := n.PlacementRequirementViewLocked()
	n.RUnlock()
	if v.GPUScope != nil {
		t.Errorf("GPUScope = %+v, want nil", v.GPUScope)
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
	deadline := time.Now().Add(10 * time.Second)
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

// lockingNodeMethods returns the names of NodeState methods (non-test files in
// this package) whose body takes the node lock, excluding the lock helpers.
func lockingNodeMethods(t *testing.T, files []*ast.File) map[string]bool {
	t.Helper()
	helpers := map[string]bool{"RLock": true, "RUnlock": true, "Lock": true, "Unlock": true}
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil || len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 {
				continue
			}
			if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); !ok {
				continue
			} else if id, ok := star.X.(*ast.Ident); !ok || id.Name != "NodeState" {
				continue
			}
			if helpers[fd.Name.Name] {
				continue
			}
			recv := fd.Recv.List[0].Names[0].Name
			ast.Inspect(fd.Body, func(x ast.Node) bool {
				sel, ok := x.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "RLock" && sel.Sel.Name != "Lock") {
					return true
				}
				switch r := sel.X.(type) {
				case *ast.Ident: // n.RLock()
					if r.Name == recv {
						out[fd.Name.Name] = true
					}
				case *ast.SelectorExpr: // n.mu.RLock()
					if id, ok := r.X.(*ast.Ident); ok && id.Name == recv && r.Sel.Name == "mu" {
						out[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return out
}

// Guard against the nested-read-lock mistake coming back. It parses the
// source, derives which node methods take the node lock themselves, and fails
// if the lock-holding builders and handlers reference any of them on the node
// variable (calls and method values alike). It only catches direct uses inside
// the named functions, not calls reached through other helpers.
func TestNoSelfLockingAccessorsUnderNodeLock(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	routerDir := filepath.Dir(thisFile)
	adminDir := filepath.Join(routerDir, "..", "admin")

	parseDir := func(dir string) []*ast.File {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		var files []*ast.File
		for _, p := range pkgs {
			for _, f := range p.Files {
				files = append(files, f)
			}
		}
		return files
	}
	routerFiles := parseDir(routerDir)
	adminFiles := parseDir(adminDir)

	selfLocking := lockingNodeMethods(t, routerFiles)
	if len(selfLocking) == 0 || !selfLocking["GetRuntime"] || !selfLocking["EffectiveRequiredGPUs"] {
		t.Fatalf("derived self-locking set looks wrong: %v", selfLocking)
	}

	targets := []struct {
		files []*ast.File
		name  string
	}{
		{adminFiles, "nodeStateToResp"},
		{adminFiles, "handleNodes"},
		{adminFiles, "handleNode"},
		{adminFiles, "newNodeDetectedExtras"},
		{routerFiles, "PlacementRequirementViewLocked"},
	}
	for _, tg := range targets {
		found := false
		for _, f := range tg.files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name.Name != tg.name || fd.Body == nil {
					continue
				}
				found = true
				ast.Inspect(fd.Body, func(x ast.Node) bool {
					sel, ok := x.(*ast.SelectorExpr)
					if !ok || !selfLocking[sel.Sel.Name] {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "n" {
						t.Errorf("%s uses self-locking n.%s while the node lock is held", tg.name, sel.Sel.Name)
					}
					return true
				})
			}
		}
		if !found {
			t.Fatalf("function %s not found", tg.name)
		}
	}
}
