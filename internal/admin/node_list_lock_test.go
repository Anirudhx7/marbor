package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
)

// buildNodeRespLocked mirrors what the node handlers do: build the response
// while holding the node's read lock. Tests call it so a change to the
// builder's signature only needs updating in one place.
func buildNodeRespLocked(s *Server, n *router.NodeState) nodeResp {
	n.RLock()
	defer n.RUnlock()
	return s.nodeStateToResp(n, "gpu-0", nil, nil)
}

func newLockTestServer(t *testing.T) (*Server, *router.NodeState) {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "RTX 4090"},
	}, nil)
	s := NewServer(r, nil, config.Config{})
	n := r.Nodes()[0]
	n.Lock()
	n.ParallelismType = "tp"
	n.DetectedParallelismType = "pp"
	n.DetectedParallelismWidth = 4
	n.DetectedGPUGroup = []int{0, 1, 2, 3}
	n.DetectedPipelineWidth = 2
	n.DetectedDataWidth = 3
	n.DetectedGPUScope = &marboragent.GPUScope{
		Indices:      []int{0, 1, 2, 3},
		UUIDs:        []string{"a", "b", "c", "d"},
		Source:       "test",
		CrossChecked: true,
	}
	n.Unlock()
	return s, n
}

func dumpGoroutines() string {
	var sb strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&sb, 1)
	return sb.String()
}

// Go's RWMutex blocks a new reader once a writer is waiting. If the response
// builder took the node's read lock a second time while the handler already
// held it, a queued writer would deadlock the node for good. This test holds
// the read lock, queues a writer, then builds the response: it must finish.
// The admin package cannot call TryRLock on the node mutex, so a short sleep
// lets the writer queue; the router package has a variant that confirms the
// queued writer exactly.
func TestNodeResp_NoDeadlockWithQueuedWriter(t *testing.T) {
	s, n := newLockTestServer(t)

	n.RLock()
	writerDone := make(chan struct{})
	go func() {
		n.Lock()
		n.Unlock()
		close(writerDone)
	}()
	time.Sleep(150 * time.Millisecond)

	done := make(chan nodeResp, 1)
	go func() {
		// Build under the lock this goroutine does not own: call the builder
		// directly because the read lock is already held by the test.
		done <- s.nodeStateToResp(n, "gpu-0", nil, nil)
	}()

	var resp nodeResp
	select {
	case resp = <-done:
	case <-time.After(2 * time.Second):
		dump := dumpGoroutines()
		n.RUnlock() // let the stuck goroutines drain
		t.Fatalf("nodeStateToResp hung with a writer queued behind the read lock\n%s", dump)
	}
	n.RUnlock()
	<-writerDone

	if resp.EffectiveRequiredGPUs != 4 {
		t.Errorf("EffectiveRequiredGPUs = %d, want 4 (verified detection, nothing declared)", resp.EffectiveRequiredGPUs)
	}
	if resp.DetectedEffectiveRequiredGPUs != 4 {
		t.Errorf("DetectedEffectiveRequiredGPUs = %d, want 4", resp.DetectedEffectiveRequiredGPUs)
	}
	if resp.DetectedPipelineWidth != 2 || resp.DetectedDataWidth != 3 {
		t.Errorf("secondary widths = %d/%d, want 2/3", resp.DetectedPipelineWidth, resp.DetectedDataWidth)
	}
	if resp.DetectedGPUScope == nil || !resp.DetectedGPUScope.CrossChecked || len(resp.DetectedGPUScope.Indices) != 4 {
		t.Errorf("DetectedGPUScope = %+v, want verified scope with 4 indices", resp.DetectedGPUScope)
	}
	if !resp.DetectedDrivesPlacement {
		t.Error("DetectedDrivesPlacement = false, want true")
	}
}

// Parity of the visible fields once a declared width and a mismatch exist.
func TestNodeResp_FieldsMatchAccessors(t *testing.T) {
	s, n := newLockTestServer(t)
	n.Lock()
	n.ParallelismWidth = 2
	n.Unlock()

	resp := buildNodeRespLocked(s, n)
	if resp.EffectiveRequiredGPUs != n.EffectiveRequiredGPUs() {
		t.Errorf("EffectiveRequiredGPUs = %d, accessor %d", resp.EffectiveRequiredGPUs, n.EffectiveRequiredGPUs())
	}
	if resp.DetectedEffectiveRequiredGPUs != n.EffectiveDetectedRequiredGPUs() {
		t.Errorf("DetectedEffectiveRequiredGPUs = %d, accessor %d", resp.DetectedEffectiveRequiredGPUs, n.EffectiveDetectedRequiredGPUs())
	}
	if resp.MismatchWarning != n.MismatchWarning() || resp.MismatchWarning == "" {
		t.Errorf("MismatchWarning = %q, accessor %q", resp.MismatchWarning, n.MismatchWarning())
	}
	if resp.DetectedDrivesPlacement != n.DetectedDrivesPlacement() {
		t.Errorf("DetectedDrivesPlacement = %v, accessor %v", resp.DetectedDrivesPlacement, n.DetectedDrivesPlacement())
	}
}

// Secondary check through the real handlers under a continuously flipping
// writer, sized by duration. The deterministic test above is the primary one.
func TestHandleNodes_NoDeadlockUnderWriter(t *testing.T) {
	s, n := newLockTestServer(t)

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n.Lock()
			n.AgentStale = !n.AgentStale
			n.Unlock()
			runtime.Gosched()
		}
	}()

	var readers sync.WaitGroup
	deadline := time.Now().Add(2 * time.Second)
	for g := 0; g < 4; g++ {
		readers.Add(1)
		go func(g int) {
			defer readers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				var rec *httptest.ResponseRecorder
				if (i+g)%2 == 0 {
					rec = httptest.NewRecorder()
					s.handleNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/nodes", nil))
				} else {
					req := httptest.NewRequest(http.MethodGet, "/admin/nodes/gpu-0", nil)
					req.SetPathValue("name", "gpu-0")
					rec = httptest.NewRecorder()
					s.handleNode(rec, req)
				}
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, body %s", rec.Code, rec.Body.String())
					return
				}
				var anyJSON any
				if err := json.Unmarshal(rec.Body.Bytes(), &anyJSON); err != nil {
					t.Errorf("bad JSON: %v", err)
					return
				}
			}
		}(g)
	}

	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		fmt.Fprintln(os.Stderr, dumpGoroutines())
		t.Fatal("node handlers hung under a concurrent writer")
	}
	close(stop)
	writer.Wait()
}
