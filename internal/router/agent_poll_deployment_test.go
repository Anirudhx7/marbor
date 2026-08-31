package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func TestMatchDeployment_PerPortIsolation(t *testing.T) {
	deps := []marboragent.DeploymentReport{
		{Runtime: "vllm", Port: 8000, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 8}, GPUGroup: []int{0, 1, 2, 3, 4, 5, 6, 7}, Source: "ps"},
		{Runtime: "vllm", Port: 8001, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 4}, GPUGroup: []int{0, 1, 2, 3}, Source: "ps"},
	}
	if d := matchDeployment(deps, "", 8000); d == nil || d.Parallelism.Width != 8 {
		t.Fatalf("port 8000: got %v want 8", d)
	}
	if d := matchDeployment(deps, "", 8001); d == nil || d.Parallelism.Width != 4 {
		t.Fatalf("port 8001: got %v want 4", d)
	}
	// Wrong port should not get the other node's deployment when both present
	if d := matchDeployment(deps, "", 9000); d != nil {
		t.Fatalf("port 9000: want nil got %v", d)
	}
}

func TestMatchDeployment_PinnedIDWins(t *testing.T) {
	deps := []marboragent.DeploymentReport{
		{RuntimeID: "id-a", Port: 8000, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 8}},
		{RuntimeID: "id-b", Port: 8001, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 4}},
	}
	if d := matchDeployment(deps, "id-b", 8000); d == nil || d.Parallelism.Width != 4 {
		t.Fatalf("pinned id-b: got %v want 4", d)
	}
}

func TestMatchDeployment_SingleUnknownPortFallback(t *testing.T) {
	deps := []marboragent.DeploymentReport{
		{Runtime: "vllm", Port: 0, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 2}},
	}
	if d := matchDeployment(deps, "", 11434); d == nil || d.Parallelism.Width != 2 {
		t.Fatalf("single unknown port: got %v want 2", d)
	}
	// Known port single deployment should NOT fallback to mismatched node port
	deps2 := []marboragent.DeploymentReport{
		{Runtime: "vllm", Port: 8000, Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 8}},
	}
	if d := matchDeployment(deps2, "", 11434); d != nil {
		t.Fatalf("mismatched port with known deployment: want nil got %v", d)
	}
}

func TestEffectiveRequired_DeclaredWinsOverDetected(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.DeclaredGPUIndices = []int{0, 1}
	n.ParallelismType = "tp"
	n.ParallelismWidth = 2
	n.DetectedGPUGroup = []int{0, 1, 2, 3, 4, 5, 6, 7}
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 8
	n.DetectedSource = "ps"
	if got := n.EffectiveRequiredGPUs(); got != 2 {
		t.Fatalf("declared wins: got %d want 2", got)
	}
	if got := n.EffectiveDetectedRequiredGPUs(); got != 8 {
		t.Fatalf("detected: got %d want 8", got)
	}
	if w := n.MismatchWarning(); w == "" {
		t.Fatalf("mismatch warning empty, want declared 2 vs detected 8")
	}
}

func TestEffectiveRequired_FallbackToDetected(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.DetectedGPUGroup = []int{0, 1}
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 2
	n.DetectedSource = "ps"
	if got := n.EffectiveRequiredGPUs(); got != 2 {
		t.Fatalf("fallback to detected: got %d want 2", got)
	}
	if got := n.MismatchWarning(); got != "" {
		t.Fatalf("no declared so no mismatch: got %q", got)
	}
}

func TestEffectiveRequired_UnconstrainedWhenBothNil(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	if got := n.EffectiveRequiredGPUs(); got != 0 {
		t.Fatalf("both nil: got %d want 0", got)
	}
}

func TestIsGPUGroupSufficient_WithDetected(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.AgentGPUs = []marboragent.GPUInfo{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}}
	n.DetectedGPUGroup = []int{0, 1, 2, 3, 4, 5, 6, 7}
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 8
	n.DetectedSource = "ps"
	// Host has 4 GPUs but deployment needs 8 => insufficient => filtered
	if r.isGPUGroupSufficient(n) {
		t.Fatalf("want insufficient (4 avail < 8 req)")
	}
	// Enough GPUs
	n.AgentGPUs = []marboragent.GPUInfo{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}, {Index: 5}, {Index: 6}, {Index: 7}}
	if !r.isGPUGroupSufficient(n) {
		t.Fatalf("want sufficient (8 avail >= 8 req)")
	}
	// Fail-open when avail 0
	n.AgentGPUs = nil
	if !r.isGPUGroupSufficient(n) {
		t.Fatalf("want fail-open when avail 0")
	}
}

func TestApplyAgentTelemetry_DeploymentPerPort(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "n8000", URL: "http://10.0.0.11:8000"},
		{Name: "n8001", URL: "http://10.0.0.11:8001", Host: "10.0.0.11"},
	}, nil)
	// Both share host 10.0.0.11
	tel := marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "host1", Version: "v1", ProtocolVersion: 1},
		Runtimes: []marboragent.RuntimeInfo{
			{Name: "vllm", Port: 8000, ID: "id-8000"},
			{Name: "vllm", Port: 8001, ID: "id-8001"},
		},
		Deployments: []marboragent.DeploymentReport{
			{Runtime: "vllm", Port: 8000, RuntimeID: "id-8000", Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 8}, GPUGroup: []int{0, 1, 2, 3, 4, 5, 6, 7}, Source: "ps"},
			{Runtime: "vllm", Port: 8001, RuntimeID: "id-8001", Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 4}, GPUGroup: []int{0, 1, 2, 3}, Source: "ps"},
		},
	}
	for _, n := range r.nodes {
		r.applyAgentTelemetry(n, tel)
	}
	// Verify per-port isolation
	for _, n := range r.nodes {
		n.mu.RLock()
		port := portOf(n.URL)
		detWidth := n.DetectedParallelismWidth
		n.mu.RUnlock()
		if port == 8000 && detWidth != 8 {
			t.Fatalf("n8000: got %d want 8", detWidth)
		}
		if port == 8001 && detWidth != 4 {
			t.Fatalf("n8001: got %d want 4", detWidth)
		}
	}
}

func TestClearAgentTelemetry_ClearsDetected(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 8
	n.DetectedGPUGroup = []int{0, 1}
	n.DetectedSource = "ps"
	clearAgentTelemetry(n)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.DetectedParallelismType != "" || n.DetectedParallelismWidth != 0 || n.DetectedGPUGroup != nil || n.DetectedSource != "" {
		t.Fatalf("cleared: got %q %d %v %q", n.DetectedParallelismType, n.DetectedParallelismWidth, n.DetectedGPUGroup, n.DetectedSource)
	}
}
