package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	out := sb.String()
	const maxDump = 8000
	if len(out) > maxDump {
		out = out[:maxDump] + "\n...(truncated)"
	}
	return out
}

// queueWriter starts a writer that blocks on the node lock and waits until it
// is provably queued behind the read lock the caller holds. The admin package
// cannot call TryRLock on the node mutex, so it probes instead: a goroutine
// that takes and drops a read lock finishes quickly while no writer is queued,
// and stays blocked once one is. A probe that finishes means "not queued yet,
// try again". A single probe that is slow could just be a busy scheduler, so
// the writer counts as queued only when two fresh probes in a row both stay
// blocked for a longer window. Every probe and the writer end once the caller
// releases its read lock; the returned function waits for all of them and must
// be called after that release.
func queueWriter(t *testing.T, n *router.NodeState) (join func()) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n.Lock()
		n.Unlock()
	}()

	const blockedWindow = 100 * time.Millisecond
	// probeBlocked starts a fresh read-lock probe and reports whether it is
	// still blocked after the window.
	probeBlocked := func() bool {
		probeDone := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.RLock()
			n.RUnlock()
			close(probeDone)
		}()
		select {
		case <-probeDone:
			return false
		case <-time.After(blockedWindow):
			return true
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if probeBlocked() && probeBlocked() {
			return wg.Wait // two consecutive probes blocked: the writer is queued
		}
		if time.Now().After(deadline) {
			n.RUnlock()
			wg.Wait()
			t.Fatal("writer never queued behind the read lock")
		}
		time.Sleep(time.Millisecond)
	}
}

// Go's RWMutex blocks a new reader once a writer is waiting. If the response
// builder took the node's read lock a second time while the handler already
// held it, a queued writer would deadlock the node for good. This test holds
// the read lock in the test goroutine, queues a writer, then runs the builder
// in a separate goroutine so a hang can be timed out: it must finish.
// The router package has a variant that confirms the queued writer exactly
// with TryRLock.
func TestNodeResp_NoDeadlockWithQueuedWriter(t *testing.T) {
	s, n := newLockTestServer(t)

	n.RLock()
	join := queueWriter(t, n)

	done := make(chan nodeResp, 1)
	go func() {
		// The read lock is held by the test goroutine, so call the builder
		// directly rather than through the locking helper.
		done <- s.nodeStateToResp(n, "gpu-0", nil, nil)
	}()

	var resp nodeResp
	select {
	case resp = <-done:
	case <-time.After(2 * time.Second):
		dump := dumpGoroutines()
		n.RUnlock() // let the stuck goroutines drain
		join()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatalf("nodeStateToResp hung with a writer queued behind the read lock\n%s", dump)
	}
	n.RUnlock()
	join()

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

	// Literal expectations for the fixture: declared 2, detected 4 (group of 4).
	if resp.EffectiveRequiredGPUs != 2 {
		t.Errorf("EffectiveRequiredGPUs = %d, want 2 (declared wins)", resp.EffectiveRequiredGPUs)
	}
	if resp.DetectedEffectiveRequiredGPUs != 4 {
		t.Errorf("DetectedEffectiveRequiredGPUs = %d, want 4", resp.DetectedEffectiveRequiredGPUs)
	}
	if want := "declared 2 GPUs vs detected 4 GPUs"; resp.MismatchWarning != want {
		t.Errorf("MismatchWarning = %q, want %q", resp.MismatchWarning, want)
	}
	if resp.DetectedDrivesPlacement {
		t.Error("DetectedDrivesPlacement = true, want false when a width is declared")
	}
	if resp.DetectedPipelineWidth != 2 || resp.DetectedDataWidth != 3 {
		t.Errorf("secondary widths = %d/%d, want 2/3", resp.DetectedPipelineWidth, resp.DetectedDataWidth)
	}
}

// A node with no detected scope must serialize no scope and no placement.
func TestNodeResp_NilScope(t *testing.T) {
	s, n := newLockTestServer(t)
	n.Lock()
	n.DetectedGPUScope = nil
	n.Unlock()

	resp := buildNodeRespLocked(s, n)
	if resp.DetectedGPUScope != nil {
		t.Errorf("DetectedGPUScope = %+v, want nil", resp.DetectedGPUScope)
	}
	if resp.DetectedDrivesPlacement {
		t.Error("DetectedDrivesPlacement = true, want false without a verified scope")
	}
	if resp.EffectiveRequiredGPUs != 0 {
		t.Errorf("EffectiveRequiredGPUs = %d, want 0 (unverified detection is not applied)", resp.EffectiveRequiredGPUs)
	}
	if resp.DetectedEffectiveRequiredGPUs != 4 {
		t.Errorf("DetectedEffectiveRequiredGPUs = %d, want 4", resp.DetectedEffectiveRequiredGPUs)
	}
}

// Secondary check through the real handlers under a continuously flipping
// writer, sized by duration. The deterministic test above is the primary one.
func TestHandleNodes_NoDeadlockUnderWriter(t *testing.T) {
	s, n := newLockTestServer(t)

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
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

	// Readers never touch t: they report their first failure on this channel
	// and the test goroutine logs it, so a reader that outlives the test (a
	// real deadlock) cannot log after the test completed.
	const readerCount = 4
	failures := make(chan string, readerCount)
	checkResp := func(r nodeResp) string {
		if r.EffectiveRequiredGPUs != 4 {
			return fmt.Sprintf("effectiveRequiredGPUs = %d, want 4", r.EffectiveRequiredGPUs)
		}
		return ""
	}
	oneRead := func(i, g int) string {
		if (i+g)%2 == 0 {
			rec := httptest.NewRecorder()
			s.handleNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/nodes", nil))
			if rec.Code != http.StatusOK {
				return fmt.Sprintf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			var list []nodeResp
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
				return fmt.Sprintf("bad list JSON (%v), len %d", err, len(list))
			}
			return checkResp(list[0])
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/nodes/gpu-0", nil)
		req.SetPathValue("name", "gpu-0")
		rec := httptest.NewRecorder()
		s.handleNode(rec, req)
		if rec.Code != http.StatusOK {
			return fmt.Sprintf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var one nodeResp
		if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
			return fmt.Sprintf("bad JSON: %v", err)
		}
		return checkResp(one)
	}

	var readers sync.WaitGroup
	deadline := time.Now().Add(2 * time.Second)
	for g := 0; g < readerCount; g++ {
		readers.Add(1)
		go func(g int) {
			defer readers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				if msg := oneRead(i, g); msg != "" {
					failures <- msg
					return
				}
			}
		}(g)
	}

	readersDone := make(chan struct{})
	go func() { readers.Wait(); close(readersDone) }()
	select {
	case <-readersDone:
		close(stop)
		<-writerDone
	case <-time.After(60 * time.Second):
		dump := dumpGoroutines()
		close(stop)
		select {
		case <-writerDone:
		case <-time.After(5 * time.Second):
		}
		var buffered []string
	drain:
		for {
			select {
			case msg := <-failures:
				buffered = append(buffered, msg)
			default:
				break drain
			}
		}
		t.Fatalf("node handlers hung under a concurrent writer; reader failures so far: %q\n%s", buffered, dump)
	}
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
}
