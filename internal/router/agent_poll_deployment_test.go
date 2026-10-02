package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, CrossChecked: true}
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
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1, 2, 3, 4, 5, 6, 7}, CrossChecked: true}
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
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, CrossChecked: true}
	n.DetectedPipelineWidth = 2
	n.DetectedDataWidth = 2
	clearAgentTelemetry(n)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.DetectedParallelismType != "" || n.DetectedParallelismWidth != 0 || n.DetectedGPUGroup != nil || n.DetectedSource != "" || n.DetectedGPUScope != nil || n.DetectedPipelineWidth != 0 || n.DetectedDataWidth != 0 {
		t.Fatalf("cleared: got %q %d %v %q", n.DetectedParallelismType, n.DetectedParallelismWidth, n.DetectedGPUGroup, n.DetectedSource)
	}
}

// A detected deployment that no independent check confirmed is information
// for the operator (to adopt), not an input to placement.
func TestEffectiveRequired_UnverifiedDetectedDoesNotDrivePlacement(t *testing.T) {
	cases := map[string]*marboragent.GPUScope{
		"no scope from agent":    nil,
		"environment only":       {Indices: []int{0, 1}, Source: "environ:CUDA_VISIBLE_DEVICES"},
		"process list mismatch":  {Indices: []int{0, 1}, Source: "environ:CUDA_VISIBLE_DEVICES", Note: "mismatch"},
		"process list only":      {Indices: []int{0, 1}, Source: "nvidia-compute-apps"},
		"explicitly not checked": {Indices: []int{0, 1}, CrossChecked: false},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
			n := r.nodes[0]
			n.AgentGPUs = []marboragent.GPUInfo{{Index: 0}}
			n.DetectedGPUGroup = []int{0, 1}
			n.DetectedParallelismType = "tp"
			n.DetectedParallelismWidth = 2
			n.DetectedGPUScope = scope
			if got := n.EffectiveRequiredGPUs(); got != 0 {
				t.Fatalf("unverified detection must not constrain placement: got %d want 0", got)
			}
			if !r.isGPUGroupSufficient(n) {
				t.Fatalf("unverified detection must not exclude the node")
			}
			if got := n.EffectiveDetectedRequiredGPUs(); got != 2 {
				t.Fatalf("operator-facing detected value must still be reported: got %d want 2", got)
			}
			if n.DetectedDrivesPlacement() {
				t.Fatalf("DetectedDrivesPlacement must be false")
			}
			n.mu.RLock()
			idx := effectiveGPUIndicesLocked(n)
			n.mu.RUnlock()
			if len(idx) != 0 {
				t.Fatalf("effective GPU indices must ignore unverified detection: %v", idx)
			}
		})
	}
}

func TestEffectiveRequired_VerifiedDetectedDrivesPlacement(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.DetectedGPUGroup = []int{2, 3}
	n.DetectedParallelismType = "tp"
	n.DetectedParallelismWidth = 2
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{2, 3}, CrossChecked: true}
	if got := n.EffectiveRequiredGPUs(); got != 2 {
		t.Fatalf("verified detection constrains placement: got %d want 2", got)
	}
	if !n.DetectedDrivesPlacement() {
		t.Fatalf("DetectedDrivesPlacement must be true")
	}
	n.mu.RLock()
	idx := effectiveGPUIndicesLocked(n)
	n.mu.RUnlock()
	if len(idx) != 2 {
		t.Fatalf("verified scope must scope indices: %v", idx)
	}
}

func TestDetectedDrivesPlacement_DeclaredAlwaysWins(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://h:11434"}}, nil)
	n := r.nodes[0]
	n.DeclaredGPUIndices = []int{0}
	n.DetectedGPUGroup = []int{0, 1}
	n.DetectedGPUScope = &marboragent.GPUScope{Indices: []int{0, 1}, CrossChecked: true}
	if n.DetectedDrivesPlacement() {
		t.Fatalf("a declared constraint means detection is not what drives placement")
	}
}

func TestApplyAgentTelemetry_CarriesGPUScopeAndSecondaryWidths(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://10.0.0.11:8000"}}, nil)
	n := r.nodes[0]
	tel := marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "host1", Version: "v1", ProtocolVersion: 1},
		Deployments: []marboragent.DeploymentReport{{
			Runtime: "vllm", Port: 8000, Source: "ps",
			Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 4, PipelineWidth: 2, DataWidth: 2},
			GPUGroup:    []int{0, 1, 2, 3},
			GPUScope:    &marboragent.GPUScope{Indices: []int{0, 1, 2, 3}, UUIDs: []string{"GPU-x"}, Raw: "0,1,2,3", Source: "environ:CUDA_VISIBLE_DEVICES", CrossChecked: true},
		}},
	}
	r.applyAgentTelemetry(n, tel)
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.DetectedPipelineWidth != 2 || n.DetectedDataWidth != 2 {
		t.Fatalf("secondary widths: %d %d", n.DetectedPipelineWidth, n.DetectedDataWidth)
	}
	if n.DetectedGPUScope == nil || !n.DetectedGPUScope.CrossChecked || n.DetectedGPUScope.Raw != "0,1,2,3" || len(n.DetectedGPUScope.UUIDs) != 1 {
		t.Fatalf("scope not carried: %+v", n.DetectedGPUScope)
	}
	// The stored scope is a copy: mutating the telemetry must not change it.
	tel.Deployments[0].GPUScope.Indices[0] = 99
	if n.DetectedGPUScope.Indices[0] == 99 {
		t.Fatalf("scope aliases the telemetry slice")
	}
}

func TestApplyAgentTelemetry_OldAgentWithoutScopeIsNotTrusted(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://10.0.0.11:8000"}}, nil)
	n := r.nodes[0]
	tel := marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "host1", Version: "v1", ProtocolVersion: 1},
		Deployments: []marboragent.DeploymentReport{{
			Runtime: "vllm", Port: 8000, Source: "ps",
			Parallelism: &marboragent.ParallelismInfo{Type: "tp", Width: 8},
			GPUGroup:    []int{0, 1, 2, 3, 4, 5, 6, 7},
		}},
	}
	r.applyAgentTelemetry(n, tel)
	if n.EffectiveRequiredGPUs() != 0 || n.DetectedDrivesPlacement() {
		t.Fatalf("an older agent's unverifiable group must not drive placement")
	}
	if n.EffectiveDetectedRequiredGPUs() != 8 {
		t.Fatalf("still shown to the operator")
	}
}

// agentWithDeployment serves a hand-built /v1/status payload carrying the
// new topology and host address fields next to one llama.cpp deployment on
// the given port, the way a newer agent would send it.
func agentWithDeployment(t *testing.T, port int, extra string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"agent": {"version": "v9.9.9", "protocol_version": 1, "platform": "linux", "architecture": "amd64"},
			"capabilities": ["status", "deployment.report", "deployment.topology"],
			"host": {"hostname": "h1", "addrs": ["10.0.0.5", "fd00::5"]},
			"health": {"runtime_reachable": true},
			"deployments": [{
				"runtime": "llamacpp", "port": %d, "source": "ps",
				"parallelism": {"type": "tp", "width": 2},
				"topology": {"launcher": "llamacpp-rpc", "rpc_servers": ["10.0.0.6:50052"], "evidence": ["cmdline:--rpc"], "future_field": 1}
				%s
			}],
			"last_updated": "2026-07-17T00:00:00Z"
		}`, port, extra)
	}))
}

func TestPollAgentTelemetryDecodesTopologyAndHostAddrs(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentWithDeployment(t, mustPort(t, psSrv.URL), "")
	defer agentSrv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{{Name: "gpu-0", URL: psSrv.URL}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, agentSrv.URL), "tok", "http")
	r.pollAgentHosts()

	n := r.nodes[0]
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.AgentPresent || n.AgentVersion != "v9.9.9" {
		t.Fatalf("poll must succeed with the new fields: present=%v version=%q", n.AgentPresent, n.AgentVersion)
	}
	if n.Hostname != "h1" {
		t.Fatalf("existing host fields must still populate: %q", n.Hostname)
	}
	if n.DetectedRuntime != "llamacpp" || n.DetectedParallelismType != "tp" || n.DetectedParallelismWidth != 2 || n.DetectedSource != "ps" {
		t.Fatalf("existing detected fields must populate: %q %q %d %q", n.DetectedRuntime, n.DetectedParallelismType, n.DetectedParallelismWidth, n.DetectedSource)
	}
}

// A llama.cpp deployment that the agent now reports follows the same rules as
// any other: a declared value always wins, and an unconfirmed detection is
// information only.
func TestNewlyReportedLlamaCppDeploymentObeysDeclaredWins(t *testing.T) {
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentWithDeployment(t, mustPort(t, psSrv.URL), "")
	defer agentSrv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{{Name: "gpu-0", URL: psSrv.URL}}, nil)
	n := r.nodes[0]
	n.DeclaredGPUIndices = []int{0, 1, 2, 3}
	n.ParallelismType = "tp"
	n.ParallelismWidth = 4
	r.SetMarborAgent(n.Host, true, mustPort(t, agentSrv.URL), "tok", "http")
	r.pollAgentHosts()

	if got := n.EffectiveRequiredGPUs(); got != 4 {
		t.Fatalf("declared must win over the detected width 2: got %d", got)
	}
	n.mu.RLock()
	declared, width, detected := append([]int(nil), n.DeclaredGPUIndices...), n.ParallelismWidth, n.DetectedParallelismWidth
	n.mu.RUnlock()
	if len(declared) != 4 || width != 4 || detected != 2 {
		t.Fatalf("declared values changed or detection missing: %v %d %d", declared, width, detected)
	}
}

func TestNewlyReportedLlamaCppDeploymentIsFallbackOnlyWhenConfirmed(t *testing.T) {
	// Nothing declared and the agent did not confirm the scope: shown, not enforced.
	psSrv := nodePSServer()
	defer psSrv.Close()
	agentSrv := agentWithDeployment(t, mustPort(t, psSrv.URL), "")
	defer agentSrv.Close()
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{{Name: "gpu-0", URL: psSrv.URL}}, nil)
	r.SetMarborAgent(r.nodes[0].Host, true, mustPort(t, agentSrv.URL), "tok", "http")
	r.pollAgentHosts()
	if got := r.nodes[0].EffectiveRequiredGPUs(); got != 0 {
		t.Fatalf("an unconfirmed detection must not drive placement: got %d", got)
	}
	if got := r.nodes[0].EffectiveDetectedRequiredGPUs(); got != 2 {
		t.Fatalf("detection must still be shown: got %d", got)
	}

	// The same report with a confirmed scope is the fallback when nothing is declared.
	confirmed := `, "gpu_group": [0, 1], "gpu_scope": {"indices": [0, 1], "cross_checked": true, "source": "environ:CUDA_VISIBLE_DEVICES"}`
	agent2 := agentWithDeployment(t, mustPort(t, psSrv.URL), confirmed)
	defer agent2.Close()
	r2 := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{{Name: "gpu-0", URL: psSrv.URL}}, nil)
	r2.SetMarborAgent(r2.nodes[0].Host, true, mustPort(t, agent2.URL), "tok", "http")
	r2.pollAgentHosts()
	if got := r2.nodes[0].EffectiveRequiredGPUs(); got != 2 {
		t.Fatalf("a confirmed detection is the fallback when nothing is declared: got %d", got)
	}
}
