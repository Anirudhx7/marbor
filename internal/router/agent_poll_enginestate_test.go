package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// TestApplyAgentTelemetry_EngineStateClearedOnNextPollMiss verifies a field
// populated by one poll must be cleared (not left stale) when a later
// poll's RuntimeInfo carries no Engine block -
// EngineState has no separate TTL, poll freshness is the only mechanism.
func TestApplyAgentTelemetry_EngineStateClearedOnNextPollMiss(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://10.0.0.11:8000"}}, nil)
	n := r.nodes[0]

	running := 3
	waiting := 1
	kv := 42.0
	telWithEngine := marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "host1", Version: "v1", ProtocolVersion: 1},
		Runtimes: []marboragent.RuntimeInfo{
			{Name: "vllm", Port: 8000, ID: "id-8000", Status: "up", Engine: &marboragent.EngineState{
				RunningRequests:     &running,
				WaitingRequests:     &waiting,
				KVCacheUsagePercent: &kv,
			}},
		},
	}
	r.applyAgentTelemetry(n, telWithEngine)

	n.mu.RLock()
	gotRunning, gotWaiting, gotKV := n.EngineRunningRequests, n.EngineWaitingRequests, n.EngineKVCacheUsagePercent
	n.mu.RUnlock()
	if gotRunning == nil || *gotRunning != 3 {
		t.Fatalf("after first poll: EngineRunningRequests = %v, want 3", gotRunning)
	}
	if gotWaiting == nil || *gotWaiting != 1 {
		t.Fatalf("after first poll: EngineWaitingRequests = %v, want 1", gotWaiting)
	}
	if gotKV == nil || *gotKV != 42 {
		t.Fatalf("after first poll: EngineKVCacheUsagePercent = %v, want 42", gotKV)
	}

	// Next poll: same runtime, no Engine block this cycle (e.g. the /metrics
	// scrape failed). Must clear, not retain the prior values.
	telWithoutEngine := marboragent.Telemetry{
		Agent: marboragent.Agent{NodeID: "host1", Version: "v1", ProtocolVersion: 1},
		Runtimes: []marboragent.RuntimeInfo{
			{Name: "vllm", Port: 8000, ID: "id-8000", Status: "up"},
		},
	}
	r.applyAgentTelemetry(n, telWithoutEngine)

	n.mu.RLock()
	gotRunning, gotWaiting, gotKV = n.EngineRunningRequests, n.EngineWaitingRequests, n.EngineKVCacheUsagePercent
	n.mu.RUnlock()
	if gotRunning != nil {
		t.Errorf("after second poll: EngineRunningRequests = %v, want nil (cleared)", *gotRunning)
	}
	if gotWaiting != nil {
		t.Errorf("after second poll: EngineWaitingRequests = %v, want nil (cleared)", *gotWaiting)
	}
	if gotKV != nil {
		t.Errorf("after second poll: EngineKVCacheUsagePercent = %v, want nil (cleared)", *gotKV)
	}
}

// TestClearAgentTelemetry_ClearsEngineState covers the full-agent-loss path
// (host disabled/unreachable) - EngineState must be wiped alongside every
// other agent-derived field, never left dangling.
func TestClearAgentTelemetry_ClearsEngineState(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{{Name: "n", URL: "http://10.0.0.11:8000"}}, nil)
	n := r.nodes[0]
	running := 5
	n.mu.Lock()
	n.EngineRunningRequests = &running
	n.mu.Unlock()

	clearAgentTelemetry(n)

	n.mu.RLock()
	got := n.EngineRunningRequests
	n.mu.RUnlock()
	if got != nil {
		t.Errorf("EngineRunningRequests = %v, want nil after clearAgentTelemetry", *got)
	}
}
