package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestHandlePatchNode_ParallelismValidationStill422(t *testing.T) {
	// P397: explicit typing mismatch len<width must be 422 (hard block)
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	st, _ := store.Open(":memory:")
	srv := NewServer(r, nil, config.Config{}, st)
	r.AddNode(config.NodeConfig{Name: "n", URL: "http://h:11434"})
	// Try patch with width 4 but gpu_indices len 2 => 422
	body := `{"gpu_indices":[0,1],"parallelism_type":"tp","parallelism_width":4}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/n", strings.NewReader(body))
	req.SetPathValue("name", "n")
	w := httptest.NewRecorder()
	srv.handlePatchNode(w, req)
	if w.Code != 422 {
		t.Fatalf("want 422 got %d body %s", w.Code, w.Body.String())
	}
}

func TestHandlePatchNode_DetectedVsDeclaredIsWarningNot422(t *testing.T) {
	// P397b: detected 8 vs declared 2 is warning amber, not 422 block.
	// Explicit patch with mismatch should succeed; mismatch is surfaced via GET.
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	st, _ := store.Open(":memory:")
	srv := NewServer(r, nil, config.Config{}, st)
	r.AddNode(config.NodeConfig{Name: "n", URL: "http://h:11434"})
	// Simulate agent discovered TP=8 via ps
	n := r.Nodes()[0]
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 8
	n.DetectedGPUGroup = []int{0, 1, 2, 3, 4, 5, 6, 7}
	n.DetectedSource = "ps"
	// Now patch declared TP=2 with group [0,1] - should succeed (200) not 422
	body := `{"gpu_indices":[0,1],"parallelism_type":"tp","parallelism_width":2}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/n", strings.NewReader(body))
	req.SetPathValue("name", "n")
	w := httptest.NewRecorder()
	srv.handlePatchNode(w, req)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d body %s", w.Code, w.Body.String())
	}
	// GET should show mismatchWarning
	req2 := httptest.NewRequest(http.MethodGet, "/admin/nodes/n", nil)
	req2.SetPathValue("name", "n")
	w2 := httptest.NewRecorder()
	srv.handleNode(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("GET want 200 got %d", w2.Code)
	}
	var resp nodeResp
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MismatchWarning == "" {
		t.Fatalf("want mismatchWarning got none, resp %+v", resp)
	}
	if resp.DetectedParallelismWidth != 8 || resp.ParallelismWidth != 2 {
		t.Fatalf("detected %d vs declared %d mismatch", resp.DetectedParallelismWidth, resp.ParallelismWidth)
	}
	if resp.EffectiveRequiredGPUs != 2 {
		t.Fatalf("effective should be declared 2, got %d", resp.EffectiveRequiredGPUs)
	}
	if resp.DetectedEffectiveRequiredGPUs != 8 {
		t.Fatalf("detectedEffective 8 got %d", resp.DetectedEffectiveRequiredGPUs)
	}
}

func TestNodeStateToResp_DetectedFallback(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	st, _ := store.Open(":memory:")
	srv := NewServer(r, nil, config.Config{}, st)
	r.AddNode(config.NodeConfig{Name: "n", URL: "http://h:11434"})
	n := r.Nodes()[0]
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 2
	n.DetectedGPUGroup = []int{0, 1}
	n.DetectedSource = "ps"
	n.DetectedRuntime = "vllm"
	n.AgentPresent = true
	// Simulate agent GPUs
	n.AgentGPUs = []marboragent.GPUInfo{{Index: 0}, {Index: 1}}
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/n", nil)
	req.SetPathValue("name", "n")
	w := httptest.NewRecorder()
	srv.handleNode(w, req)
	var resp nodeResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.DetectedParallelismType != "tp" || resp.DetectedSource != "ps" {
		t.Fatalf("detected not exposed: %+v", resp)
	}
	if resp.EffectiveRequiredGPUs != 2 {
		t.Fatalf("effective should fallback to detected 2, got %d", resp.EffectiveRequiredGPUs)
	}
}
