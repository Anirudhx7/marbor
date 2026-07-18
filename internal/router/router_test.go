package router

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestRouteWarmFirst(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "RTX 4090"},
		{Name: "gpu-1", URL: "http://localhost:2", GPUModel: "RTX 4090"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama3.2:8b"}}
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].mu.Unlock()

	node, warm := r.Route("llama3.2:8b", "", "")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Name != "gpu-0" {
		t.Errorf("route = %s, want gpu-0", node.Name)
	}
	if !warm {
		t.Error("expected warm routing")
	}

	node, warm = r.Route("unknown", "", "")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if warm {
		t.Error("expected cold routing")
	}
}

func TestExtractModelName(t *testing.T) {
	body := []byte(`{"model":"llama3.2:8b","prompt":"hi"}`)
	got := ExtractModelName(body)
	if got != "llama3.2:8b" {
		t.Errorf("model = %q, want llama3.2:8b", got)
	}
}

func TestAllUnhealthy(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()
	node, _ := r.Route("any", "", "")
	if node != nil {
		t.Error("expected nil for all unhealthy")
	}
}

func TestWarmFirstPicksLeastConns(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "RTX 4090"},
		{Name: "gpu-1", URL: "http://localhost:2", GPUModel: "RTX 4090"},
	}, nil)
	model := []ModelInfo{{Name: "llama3.2:8b"}}
	r.nodes[0].mu.Lock()
	r.nodes[0].LoadedModels = model
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].LoadedModels = model
	r.nodes[1].Healthy = true
	r.nodes[1].mu.Unlock()

	// gpu-0 has more active connections - gpu-1 should win
	atomic.StoreInt32(&r.nodes[0].ActiveConns, 5)
	atomic.StoreInt32(&r.nodes[1].ActiveConns, 1)

	node, warm := r.Route("llama3.2:8b", "", "")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Name != "gpu-1" {
		t.Errorf("route = %s, want gpu-1 (least conns)", node.Name)
	}
	if !warm {
		t.Error("expected warm routing")
	}
}

func TestPollNodeWithMockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		// Ollama's /api/ps sends size_vram (snake_case). Using the real key guards
		// the decode-tag bug where the field was silently dropped to 0.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3.2:8b", "size_vram": 4294967296},
			},
		})
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL, GPUModel: "RTX 4090"},
	}, nil)

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()

	if !r.nodes[0].Healthy {
		t.Error("expected node healthy after successful poll")
	}
	if len(r.nodes[0].LoadedModels) != 1 {
		t.Fatalf("expected 1 loaded model, got %d", len(r.nodes[0].LoadedModels))
	}
	if r.nodes[0].LoadedModels[0].Name != "llama3.2:8b" {
		t.Errorf("model = %s, want llama3.2:8b", r.nodes[0].LoadedModels[0].Name)
	}
	if r.nodes[0].LoadedModels[0].SizeVRAM != 4294967296 {
		t.Errorf("SizeVRAM = %d, want 4294967296 (size_vram must decode)", r.nodes[0].LoadedModels[0].SizeVRAM)
	}
	// With no nvidia-smi available (CI), used-VRAM is derived from /api/ps:
	// 4294967296 bytes / 1MiB = 4096 MB. This is the real cross-cluster signal.
	if r.nodes[0].VRAMUsedMB != 4096 {
		t.Errorf("VRAMUsedMB = %d, want 4096 (summed from /api/ps size_vram)", r.nodes[0].VRAMUsedMB)
	}
	if r.nodes[0].LastPollAt.IsZero() {
		t.Error("expected LastPollAt to be set")
	}
}

// TestRemoteNodeVRAMTelemetry verifies the agentless cross-cluster telemetry:
// a remote node (non-localhost URL, so nvidia-smi is never attributed to it)
// reports real used-VRAM summed from its own /api/ps, an operator-declared total,
// and the honest "declared" source label.
func TestRemoteNodeVRAMTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3.2:8b", "size_vram": 4294967296}, // 4096 MB
				{"name": "mistral:7b", "size_vram": 4294967296},  // 4096 MB
			},
		})
	}))
	defer srv.Close()

	// The test/CI environment (golang container) has no nvidia-smi, so queryGPU
	// returns hasGPU=false and the declared-total branch is exercised regardless of
	// the localhost test-server URL. isLocalNode's remote-vs-local logic is covered
	// independently by TestIsLocalNode.
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-remote", URL: srv.URL, GPUModel: "A10G", VRAMTotalMB: 24576},
	}, nil)

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()

	// Two models at 4096 MB each => 8192 MB used, summed from /api/ps.
	if r.nodes[0].VRAMUsedMB != 8192 {
		t.Errorf("VRAMUsedMB = %d, want 8192", r.nodes[0].VRAMUsedMB)
	}
	// Declared total is surfaced (CI has no nvidia-smi, so the declared branch wins).
	if r.nodes[0].VRAMTotalMB != 24576 {
		t.Errorf("VRAMTotalMB = %d, want 24576 (operator-declared)", r.nodes[0].VRAMTotalMB)
	}
	if r.nodes[0].VRAMSource != "declared" {
		t.Errorf("VRAMSource = %q, want \"declared\"", r.nodes[0].VRAMSource)
	}
}

func TestIsLocalNode(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:11434":  true,
		"http://127.0.0.1:11434":  true,
		"http://[::1]:11434":      true,
		"http://0.0.0.0:11434":    true,
		"http://10.0.1.10:11434":  false,
		"http://gpu-remote:11434": false,
		"http://example.com":      false,
	}
	for url, want := range cases {
		if got := isLocalNode(url); got != want {
			t.Errorf("isLocalNode(%q) = %v, want %v", url, got, want)
		}
	}
}

// TestIsLocalNodeMatchesOwnLANInterfaceIP verifies that a node configured with
// this machine's actual LAN IP (rather than localhost/127.0.0.1) is still
// recognized as local, so it does not silently lose local nvidia-smi
// telemetry and thermal-watchdog auto-drain coverage.
func TestIsLocalNodeMatchesOwnLANInterfaceIP(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("net.InterfaceAddrs() error: %v", err)
	}
	var ownIP string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ownIP = ipNet.IP.String()
		break
	}
	if ownIP == "" {
		t.Skip("no non-loopback interface address available in this environment")
	}

	url := "http://" + ownIP + ":11434"
	if got := isLocalNode(url); !got {
		t.Errorf("isLocalNode(%q) = %v, want true (matches this machine's own interface IP)", url, got)
	}

	// A LAN IP that is not one of this machine's own addresses must still be
	// treated as remote.
	if got := isLocalNode("http://192.0.2.123:11434"); got {
		t.Errorf("isLocalNode(%q) = %v, want false (not this machine's IP)", "http://192.0.2.123:11434", got)
	}
}

func TestPollNodeMarksUnhealthyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	}, nil)

	// 3 failures needed to mark unhealthy
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	healthy := r.nodes[0].Healthy
	failures := r.nodes[0].Failures
	r.nodes[0].mu.RUnlock()

	if healthy {
		t.Error("expected node unhealthy after 3 failed polls")
	}
	if failures < 3 {
		t.Errorf("failures = %d, want >= 3", failures)
	}
}

func TestPollNodeRecoveryRequiresConsecutiveSuccessThreshold(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{
		PollIntervalMs:         2000,
		HealthFailureThreshold: 3,
		HealthSuccessThreshold: 2,
	}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	}, nil)

	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])
	r.pollNode(r.nodes[0])
	r.nodes[0].mu.RLock()
	healthy := r.nodes[0].Healthy
	r.nodes[0].mu.RUnlock()
	if healthy {
		t.Fatal("expected node unhealthy after 3 failed polls")
	}

	failing.Store(false)

	// First successful poll after the outage: still under the 2-poll
	// threshold, must stay unhealthy (not put back into rotation on one
	// lucky poll).
	r.pollNode(r.nodes[0])
	r.nodes[0].mu.RLock()
	healthy = r.nodes[0].Healthy
	successes := r.nodes[0].ConsecutiveSuccesses
	r.nodes[0].mu.RUnlock()
	if healthy {
		t.Error("expected node still unhealthy after only 1 consecutive success (threshold=2)")
	}
	if successes != 1 {
		t.Errorf("ConsecutiveSuccesses = %d, want 1", successes)
	}

	// Second consecutive success crosses the threshold.
	r.pollNode(r.nodes[0])
	r.nodes[0].mu.RLock()
	healthy = r.nodes[0].Healthy
	r.nodes[0].mu.RUnlock()
	if !healthy {
		t.Error("expected node healthy after 2 consecutive successes (threshold=2)")
	}
}

func TestRouteCloudNilWhenNoProviders(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, nil)
	if got := r.RouteCloud(); got != nil {
		t.Errorf("RouteCloud() = %v, want nil when no providers", got)
	}
}

func TestRouteCloudNilWhenDisabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-test", Enabled: false},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, clouds)
	if got := r.RouteCloud(); got != nil {
		t.Errorf("RouteCloud() = %v, want nil when provider disabled", got)
	}
}

func TestRouteCloudReturnsFirstEnabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "disabled-one", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-a", Enabled: false},
		{Name: "enabled-one", Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-b", Enabled: true},
		{Name: "enabled-two", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-c", Enabled: true},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, clouds)
	got := r.RouteCloud()
	if got == nil {
		t.Fatal("RouteCloud() = nil, want first enabled provider")
	}
	if got.Name != "enabled-one" {
		t.Errorf("RouteCloud().Name = %q, want enabled-one", got.Name)
	}
}

func TestRouteCloudFallsBackWhenAllNodesUnhealthy(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-test", Enabled: true},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, clouds)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()

	node, _ := r.Route("llama3.2:8b", "", "")
	if node != nil {
		t.Error("Route() should return nil when all nodes unhealthy")
	}
	cloud := r.RouteCloud()
	if cloud == nil {
		t.Fatal("RouteCloud() = nil, want enabled provider as fallback")
	}
	if cloud.Name != "openai" {
		t.Errorf("RouteCloud().Name = %q, want openai", cloud.Name)
	}
}

func TestSessionAffinitySticksToBestNode(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, SessionAffinity: true}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
		{Name: "node-b", URL: "http://node-b:11434"},
	}, nil)
	// Both nodes healthy, node-a has the model warm.
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama3"}}
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].mu.Unlock()

	// First request: no affinity entry yet - should pick node-a (warm).
	n1, warm1 := r.Route("llama3", "sess-1", "")
	if n1 == nil {
		t.Fatal("expected a node, got nil")
	}
	if n1.Name != "node-a" {
		t.Errorf("first Route() = %s, want node-a (warm)", n1.Name)
	}
	if !warm1 {
		t.Error("expected warm=true for first route")
	}

	// Second request: affinity pinned to node-a - must return node-a even if node-b is also warm.
	r.nodes[1].mu.Lock()
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "llama3"}}
	r.nodes[1].mu.Unlock()
	n2, _ := r.Route("llama3", "sess-1", "")
	if n2 == nil || n2.Name != "node-a" {
		t.Errorf("second Route() = %v, want node-a (sticky)", n2)
	}
}

func TestSessionAffinityFallsBackOnUnhealthyNode(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, SessionAffinity: true}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
		{Name: "node-b", URL: "http://node-b:11434"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "llama3"}}
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "llama3"}}
	r.nodes[1].mu.Unlock()

	// Pin sess-2 to node-a.
	r.Route("llama3", "sess-2", "")

	// node-a goes down.
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = false
	r.nodes[0].mu.Unlock()

	// Next request for sess-2 must not return nil or node-a.
	n, _ := r.Route("llama3", "sess-2", "")
	if n == nil {
		t.Fatal("expected fallback node, got nil")
	}
	if n.Name == "node-a" {
		t.Error("sticky node was unhealthy but Route still returned it")
	}
	// Affinity entry for the old (unhealthy) node must have been evicted.
	// After re-routing, a new entry for node-b may exist (correct); only a
	// surviving pin to node-a's URL is wrong. Read nodeURL inside the lock.
	r.affinityMu.RLock()
	var pinnedURL string
	if e, ok := r.affinity["sess-2"]; ok {
		pinnedURL = e.nodeURL
	}
	r.affinityMu.RUnlock()
	if pinnedURL == "http://node-a:11434" {
		t.Error("stale affinity entry for unhealthy node-a was not evicted")
	}
}

func TestSessionAffinityNoIDIsStateless(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, SessionAffinity: true}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()

	// No session ID - must not create affinity entries.
	r.Route("llama3", "", "")
	r.affinityMu.RLock()
	count := len(r.affinity)
	r.affinityMu.RUnlock()
	if count != 0 {
		t.Errorf("affinity map has %d entries after stateless Route(), want 0", count)
	}
}

// TestSessionAffinityDisabledIgnoresSessionID verifies the flag actually gates
// pinning: with session_affinity off (the default), a session ID must be
// ignored and create no sticky entry.
func TestSessionAffinityDisabledIgnoresSessionID(t *testing.T) {
	// Note: SessionAffinity defaults to false here (not set).
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()

	r.Route("llama3", "sess-x", "")
	r.Route("llama3", "sess-x", "")
	r.affinityMu.RLock()
	count := len(r.affinity)
	r.affinityMu.RUnlock()
	if count != 0 {
		t.Errorf("affinity map has %d entries with session_affinity disabled, want 0", count)
	}
}

func TestSweepAffinityRemovesExpired(t *testing.T) {
	r := New(config.RoutingConfig{SessionAffinityTTL: "1s"}, []config.NodeConfig{}, nil)
	// Insert a stale entry.
	r.affinityMu.Lock()
	oldEntry := &affinityEntry{nodeURL: "http://x:11434"}
	oldEntry.lastSeen.Store(time.Now().Add(-2 * time.Second).UnixNano())
	r.affinity["old"] = oldEntry

	freshEntry := &affinityEntry{nodeURL: "http://y:11434"}
	freshEntry.lastSeen.Store(time.Now().UnixNano())
	r.affinity["fresh"] = freshEntry
	r.affinityMu.Unlock()

	r.sweepAffinity()

	r.affinityMu.RLock()
	_, oldStillPresent := r.affinity["old"]
	_, freshStillPresent := r.affinity["fresh"]
	r.affinityMu.RUnlock()

	if oldStillPresent {
		t.Error("expired entry 'old' should have been removed by sweepAffinity")
	}
	if !freshStillPresent {
		t.Error("fresh entry should not have been removed by sweepAffinity")
	}
}

func TestPickMostFreeVRAM_BasicSelection(t *testing.T) {
	// Three nodes: free VRAM = 4000, 14000, 15000 - third wins.
	nodes := make([]*NodeState, 3)
	for i := range nodes {
		nodes[i] = &NodeState{}
	}
	nodes[0].mu.Lock()
	nodes[0].VRAMTotalMB = 24000
	nodes[0].VRAMUsedMB = 20000
	nodes[0].mu.Unlock()
	nodes[1].mu.Lock()
	nodes[1].VRAMTotalMB = 24000
	nodes[1].VRAMUsedMB = 10000
	nodes[1].mu.Unlock()
	nodes[2].mu.Lock()
	nodes[2].VRAMTotalMB = 16000
	nodes[2].VRAMUsedMB = 1000
	nodes[2].mu.Unlock()

	got := pickMostFreeVRAM(nodes)
	if got != nodes[2] {
		t.Errorf("pickMostFreeVRAM returned wrong node; want nodes[2] (15000 MB free)")
	}
}

func TestPickMostFreeVRAM_AllUnknown(t *testing.T) {
	// All nodes have VRAMTotalMB==0; must fall back to pickLeastConns (non-nil result).
	nodes := make([]*NodeState, 2)
	for i := range nodes {
		nodes[i] = &NodeState{}
	}
	nodes[0].mu.Lock()
	nodes[0].VRAMTotalMB = 0
	nodes[0].mu.Unlock()
	atomic.StoreInt32(&nodes[0].ActiveConns, 5)
	nodes[1].mu.Lock()
	nodes[1].VRAMTotalMB = 0
	nodes[1].mu.Unlock()
	atomic.StoreInt32(&nodes[1].ActiveConns, 1)

	got := pickMostFreeVRAM(nodes)
	if got == nil {
		t.Fatal("pickMostFreeVRAM returned nil; want least-conns fallback")
	}
	// pickLeastConns should pick nodes[1] (1 active conn).
	if got != nodes[1] {
		t.Errorf("all-unknown fallback: got %p, want nodes[1] (least conns)", got)
	}
}

func TestPickMostFreeVRAM_MixedUnknown(t *testing.T) {
	// One node with unknown capacity is skipped; best known returned.
	nodes := make([]*NodeState, 3)
	for i := range nodes {
		nodes[i] = &NodeState{}
	}
	nodes[0].mu.Lock()
	nodes[0].VRAMTotalMB = 0 // unknown - skipped
	nodes[0].mu.Unlock()
	nodes[1].mu.Lock()
	nodes[1].VRAMTotalMB = 24000
	nodes[1].VRAMUsedMB = 20000 // 4000 free
	nodes[1].mu.Unlock()
	nodes[2].mu.Lock()
	nodes[2].VRAMTotalMB = 24000
	nodes[2].VRAMUsedMB = 8000 // 16000 free - wins
	nodes[2].mu.Unlock()

	got := pickMostFreeVRAM(nodes)
	if got != nodes[2] {
		t.Errorf("mixed-unknown: want nodes[2] (most free known VRAM), got %p", got)
	}
}

func TestRouteInternal_VRAMFallback(t *testing.T) {
	// fallback="vram-aware", no warm models - VRAM free should win over conn count.
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "vram-aware", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 24576},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 24576},
	}, nil)
	// node-a: more active conns but more free VRAM.
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].VRAMTotalMB = 24576
	r.nodes[0].VRAMUsedMB = 4096 // 20480 free
	r.nodes[0].mu.Unlock()
	atomic.StoreInt32(&r.nodes[0].ActiveConns, 10)

	// node-b: fewer conns but less free VRAM.
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].VRAMTotalMB = 24576
	r.nodes[1].VRAMUsedMB = 20000 // 4576 free
	r.nodes[1].mu.Unlock()
	atomic.StoreInt32(&r.nodes[1].ActiveConns, 1)

	node, warm := r.Route("unknown-model", "", "")
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if warm {
		t.Error("expected warm=false (no loaded models)")
	}
	if node.Name != "node-a" {
		t.Errorf("vram-aware fallback: got %s, want node-a (most free VRAM)", node.Name)
	}
}

func TestRouteExcluding_VRAMFallback(t *testing.T) {
	// Same setup but node-a is excluded; node-b must be returned.
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "vram-aware", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "node-a", URL: "http://node-a:11434", VRAMTotalMB: 24576},
		{Name: "node-b", URL: "http://node-b:11434", VRAMTotalMB: 24576},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].VRAMTotalMB = 24576
	r.nodes[0].VRAMUsedMB = 4096
	r.nodes[0].mu.Unlock()
	atomic.StoreInt32(&r.nodes[0].ActiveConns, 10)

	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].VRAMTotalMB = 24576
	r.nodes[1].VRAMUsedMB = 20000
	r.nodes[1].mu.Unlock()
	atomic.StoreInt32(&r.nodes[1].ActiveConns, 1)

	exclude := map[string]bool{"http://node-a:11434": true}
	node, _ := r.RouteExcluding("unknown-model", "", exclude)
	if node == nil {
		t.Fatal("expected node, got nil")
	}
	if node.Name != "node-b" {
		t.Errorf("RouteExcluding with vram-aware: got %s, want node-b (only non-excluded)", node.Name)
	}
}

func TestPollNodeUptime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer srv.Close()

	r := New(config.RoutingConfig{PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "gpu-0", URL: srv.URL},
	}, nil)
	r.nodes[0].FirstSeenAt = time.Now().Add(-2 * time.Hour)

	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	uptime := r.nodes[0].Uptime
	r.nodes[0].mu.RUnlock()

	if uptime == "" {
		t.Error("expected Uptime to be set")
	}
}

func TestDrainNodeExcludesFromRouting(t *testing.T) {
	cfg := config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 100}
	nodes := []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434"},
		{Name: "node-b", URL: "http://localhost:11435"},
	}
	r := New(cfg, nodes, nil)

	// Mark both nodes healthy and node-b as having the model warm.
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].LoadedModels = []ModelInfo{{Name: "llama3", SizeVRAM: 1024}}
	r.nodes[1].mu.Unlock()

	// Drain node-b.
	if !r.DrainNode("node-b", "manual") {
		t.Fatal("DrainNode returned false for existing node")
	}

	// Route should never return node-b while draining.
	for i := 0; i < 10; i++ {
		node, _ := r.Route("llama3", "", "")
		if node == nil {
			t.Fatal("Route returned nil (expected node-a)")
		}
		if node.Name == "node-b" {
			t.Error("draining node-b was selected by router")
		}
	}

	// Undrain restores it to the pool.
	if !r.UndrainNode("node-b") {
		t.Fatal("UndrainNode returned false for existing node")
	}
	r.nodes[1].mu.RLock()
	draining := r.nodes[1].Draining
	r.nodes[1].mu.RUnlock()
	if draining {
		t.Error("Draining should be false after UndrainNode")
	}
}

func TestSyncNodes(t *testing.T) {
	cfg := config.RoutingConfig{}
	initial := []config.NodeConfig{
		{Name: "a", URL: "http://a:11434"},
		{Name: "b", URL: "http://b:11434"},
	}
	r := New(cfg, initial, nil)
	if len(r.nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(r.nodes))
	}

	// Remove b, add c, keep a.
	newNodes := []config.NodeConfig{
		{Name: "a", URL: "http://a:11434"},
		{Name: "c", URL: "http://c:11434"},
	}
	added, removed := r.SyncNodes(newNodes)
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	r.mu.RLock()
	names := make([]string, len(r.nodes))
	for i, n := range r.nodes {
		names[i] = n.Name
	}
	r.mu.RUnlock()
	foundA, foundC, foundB := false, false, false
	for _, n := range names {
		switch n {
		case "a":
			foundA = true
		case "c":
			foundC = true
		case "b":
			foundB = true
		}
	}
	if !foundA {
		t.Error("node a should still be present")
	}
	if !foundC {
		t.Error("node c should have been added")
	}
	if foundB {
		t.Error("node b should have been removed")
	}
}

// TestAddNode_RejectsDuplicateURLUnderDifferentName reproduces a real-world
// bug scenario: a node ("pve") is already registered (as if loaded from
// config.yaml at router construction time), and a second source - the SQLite
// store overlay in main.go, the admin "add node" API, or Docker
// auto-discovery, all of which funnel through Router.AddNode - later tries to
// register the exact same physical backend under a different auto-generated
// name ("discovered-ollama-1", the name install.sh's PROBE-based discovery
// writes into a freshly generated config.yaml). Without a URL-based dedup
// check, both survive as independent, fully-routable NodeStates that appear
// to double the mesh's real capacity and split that node's usage/eviction
// accounting in two. AddNode must reject the second registration and keep
// only the first-seen node.
func TestAddNode_RejectsDuplicateURLUnderDifferentName(t *testing.T) {
	cfg := config.RoutingConfig{}
	// Simulates router.New(cfg.Nodes, ...) loading "pve" from config.yaml.
	initial := []config.NodeConfig{
		{Name: "pve", URL: "http://192.168.1.115:11434"},
	}
	r := New(cfg, initial, nil)

	// Simulates the DB-store overlay in main.go calling r.AddNode() for a
	// runtime/discovered node whose URL is cosmetically different (trailing
	// slash) but resolves to the identical backend.
	r.AddNode(config.NodeConfig{Name: "discovered-ollama-1", URL: "http://192.168.1.115:11434/"})

	nodes := r.Nodes()
	if len(nodes) != 1 {
		names := make([]string, len(nodes))
		for i, n := range nodes {
			names[i] = n.Name
		}
		t.Fatalf("expected 1 live node after duplicate-URL AddNode, got %d: %v", len(nodes), names)
	}
	if nodes[0].Name != "pve" {
		t.Errorf("expected first-seen node %q to survive, got %q", "pve", nodes[0].Name)
	}
}

// TestAddNode_AllowsDistinctURLs is the negative case: two genuinely
// different backends must both be added normally.
func TestAddNode_AllowsDistinctURLs(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "pve", URL: "http://192.168.1.115:11434"},
	}, nil)
	r.AddNode(config.NodeConfig{Name: "other-box", URL: "http://192.168.1.116:11434"})
	if len(r.Nodes()) != 2 {
		t.Fatalf("expected 2 live nodes for distinct URLs, got %d", len(r.Nodes()))
	}
}

func TestDrainNodeNotFound(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	if r.DrainNode("nonexistent", "manual") {
		t.Error("DrainNode should return false for unknown node")
	}
	if r.UndrainNode("nonexistent") {
		t.Error("UndrainNode should return false for unknown node")
	}
}

func TestSetPrewarmDisabled_NotFound(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	if r.SetPrewarmDisabled("nonexistent", true) {
		t.Error("SetPrewarmDisabled should return false for unknown node")
	}
}

// TestSetPrewarmDisabled_ExcludesFromPredictionCycle verifies that a node
// with prewarm disabled is skipped by the predictive engine as a warmup
// target, while remaining otherwise unaffected (live traffic is untouched -
// this is not the same as Draining).
func TestSetPrewarmDisabled_ExcludesFromPredictionCycle(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, []config.NodeConfig{
		{Name: "node-a", URL: "http://localhost:11434", VRAMTotalMB: 16384},
	}, nil)
	r.SetWarmupConfig(config.WarmupConfig{Enabled: true, IntervalMs: 300000})

	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].LoadedModels = []ModelInfo{{Name: "model-w", SizeVRAM: 2000 * 1024 * 1024}}
	r.nodes[0].VRAMTotalMB = 16384
	r.nodes[0].VRAMUsedMB = 2000
	r.nodes[0].mu.Unlock()

	if !r.SetPrewarmDisabled("node-a", true) {
		t.Fatal("SetPrewarmDisabled returned false for existing node")
	}

	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	r.RecordTransition("model-w", now)
	r.RecordTransition("model-x", now)
	r.RecordTransition("model-w", now)

	r.RunPredictionCycle(context.Background(), now)

	decisions := r.RecentPredictiveDecisions()
	for _, d := range decisions {
		if d.Node == "node-a" {
			t.Errorf("expected no predictive decisions for prewarm-disabled node-a, got %+v", d)
		}
	}

	if !r.SetPrewarmDisabled("node-a", false) {
		t.Fatal("SetPrewarmDisabled(false) returned false for existing node")
	}
}

func TestPatchNodeMetadata(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)

	r.nodes[0].mu.Lock()
	r.nodes[0].VRAMSource = "none"
	r.nodes[0].mu.Unlock()

	vram := int64(24576)
	model := "NVIDIA RTX 4090"
	if !r.PatchNode("gpu-0", NodePatch{VRAMTotalMB: &vram, GPUModel: &model}) {
		t.Fatal("PatchNode returned false for existing node")
	}

	r.nodes[0].mu.RLock()
	gotVRAM := r.nodes[0].VRAMTotalMB
	gotConfig := r.nodes[0].VRAMTotalMBConfig
	gotSource := r.nodes[0].VRAMSource
	gotModel := r.nodes[0].GPUModel
	r.nodes[0].mu.RUnlock()

	if gotVRAM != 24576 {
		t.Errorf("VRAMTotalMB = %d, want 24576", gotVRAM)
	}
	if gotConfig != 24576 {
		t.Errorf("VRAMTotalMBConfig = %d, want 24576", gotConfig)
	}
	if gotSource != "declared" {
		t.Errorf("VRAMSource = %q, want declared", gotSource)
	}
	if gotModel != "NVIDIA RTX 4090" {
		t.Errorf("GPUModel = %q, want NVIDIA RTX 4090", gotModel)
	}
}

func TestPatchNodeRuntime(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "ollama"},
	}, nil)

	vllm := "vllm"
	if !r.PatchNode("gpu-0", NodePatch{Runtime: &vllm}) {
		t.Fatal("PatchNode returned false for existing node")
	}
	r.nodes[0].mu.RLock()
	gotRuntime := r.nodes[0].Runtime
	gotAutoDetect := r.nodes[0].autoDetect
	r.nodes[0].mu.RUnlock()
	if gotRuntime != "vllm" {
		t.Errorf("Runtime = %q, want vllm", gotRuntime)
	}
	if gotAutoDetect {
		t.Error("autoDetect should be false after patching to an explicit runtime")
	}

	auto := "auto"
	if !r.PatchNode("gpu-0", NodePatch{Runtime: &auto}) {
		t.Fatal("PatchNode returned false for existing node")
	}
	r.nodes[0].mu.RLock()
	gotRuntime = r.nodes[0].Runtime
	gotAutoDetect = r.nodes[0].autoDetect
	r.nodes[0].mu.RUnlock()
	if gotRuntime != "auto" {
		t.Errorf("Runtime = %q, want auto", gotRuntime)
	}
	if !gotAutoDetect {
		t.Error("autoDetect should be re-armed after patching runtime back to auto")
	}
}

// TestPatchNodeRuntime_ExplicitFromPendingAutoDetectSetsProbe guards a nil
// pointer panic in pollNode: a node created as Runtime: "auto" has a nil
// probe until its first successful detection. Patching it straight to an
// explicit runtime before that detection ever ran must set a matching
// probe immediately - otherwise autoDetect goes false with probe still
// nil, pollNode's needsDetect guard never re-arms, and the very next poll
// dereferences a nil probe and crashes the whole (single-process) mesh.
func TestPatchNodeRuntime_ExplicitFromPendingAutoDetectSetsProbe(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434", Runtime: "auto"},
	}, nil)

	r.nodes[0].mu.RLock()
	if r.nodes[0].probe != nil {
		t.Fatal("test setup invariant broken: expected nil probe before first auto-detect")
	}
	r.nodes[0].mu.RUnlock()

	ollama := "ollama"
	if !r.PatchNode("gpu-0", NodePatch{Runtime: &ollama}) {
		t.Fatal("PatchNode returned false for existing node")
	}

	r.nodes[0].mu.RLock()
	probe := r.nodes[0].probe
	autoDetect := r.nodes[0].autoDetect
	r.nodes[0].mu.RUnlock()

	if autoDetect {
		t.Error("autoDetect should be false after patching to an explicit runtime")
	}
	if probe == nil {
		t.Fatal("probe must not be nil after patching an auto-detect-pending node to an explicit runtime - the next poll would nil-panic")
	}

	// pollNode must not panic now that both flags agree.
	r.pollNode(r.nodes[0])
}

func TestPatchNodeSkipsVRAMWhenNvidia(t *testing.T) {
	r := New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://gpu-0:11434"},
	}, nil)

	r.nodes[0].mu.Lock()
	r.nodes[0].VRAMSource = "nvidia"
	r.nodes[0].VRAMTotalMB = 40960
	r.nodes[0].mu.Unlock()

	vram := int64(8192)
	if !r.PatchNode("gpu-0", NodePatch{VRAMTotalMB: &vram}) {
		t.Fatal("PatchNode returned false")
	}

	r.nodes[0].mu.RLock()
	gotLive := r.nodes[0].VRAMTotalMB
	gotConfig := r.nodes[0].VRAMTotalMBConfig
	gotSource := r.nodes[0].VRAMSource
	r.nodes[0].mu.RUnlock()

	// Config field updated but live total must not change when nvidia owns it.
	if gotConfig != 8192 {
		t.Errorf("VRAMTotalMBConfig = %d, want 8192", gotConfig)
	}
	if gotLive != 40960 {
		t.Errorf("VRAMTotalMB changed to %d; should stay 40960 when source=nvidia", gotLive)
	}
	if gotSource != "nvidia" {
		t.Errorf("VRAMSource changed to %q; should stay nvidia", gotSource)
	}
}

func TestPatchNodeNotFound(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	vram := int64(8192)
	if r.PatchNode("nonexistent", NodePatch{VRAMTotalMB: &vram}) {
		t.Error("PatchNode should return false for unknown node")
	}
}

// TestRoute_RuntimeFilter_OllamaOnly verifies that runtimeFilter="ollama" returns
// only the Ollama node when both an Ollama and a vLLM node are healthy.
func TestRoute_RuntimeFilter_OllamaOnly(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "ollama-0", URL: "http://localhost:1"},
		{Name: "vllm-0", URL: "http://localhost:2"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].Runtime = "ollama"
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].Runtime = "vllm"
	r.nodes[1].mu.Unlock()

	node, _ := r.Route("llama3", "", "ollama")
	if node == nil {
		t.Fatal("expected a node, got nil")
	}
	if node.Name != "ollama-0" {
		t.Errorf("Route with runtimeFilter=ollama returned %q, want ollama-0", node.Name)
	}
}

// TestRoute_RuntimeFilter_Any verifies that runtimeFilter="" allows routing to
// any runtime - both Ollama and vLLM nodes are eligible.
func TestRoute_RuntimeFilter_Any(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "ollama-0", URL: "http://localhost:1"},
		{Name: "vllm-0", URL: "http://localhost:2"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].Runtime = "ollama"
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].Runtime = "vllm"
	r.nodes[1].mu.Unlock()

	node, _ := r.Route("llama3", "", "")
	if node == nil {
		t.Fatal("Route with runtimeFilter=\"\" returned nil, want any node")
	}
}

// TestRoute_RuntimeFilter_NoMatch verifies that runtimeFilter="ollama" returns
// nil when only vLLM nodes are available.
func TestRoute_RuntimeFilter_NoMatch(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "vllm-0", URL: "http://localhost:1"},
		{Name: "vllm-1", URL: "http://localhost:2"},
	}, nil)
	r.nodes[0].mu.Lock()
	r.nodes[0].Healthy = true
	r.nodes[0].Runtime = "vllm"
	r.nodes[0].mu.Unlock()
	r.nodes[1].mu.Lock()
	r.nodes[1].Healthy = true
	r.nodes[1].Runtime = "vllm"
	r.nodes[1].mu.Unlock()

	node, _ := r.Route("llama3", "", "ollama")
	if node != nil {
		t.Errorf("Route with runtimeFilter=ollama returned %q, want nil (no Ollama nodes)", node.Name)
	}
}

func TestCloudChainOrdersByPriorityDescending(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "low", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-a", Enabled: true, Priority: 1},
		{Name: "high", Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-b", Enabled: true, Priority: 10},
		{Name: "mid", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-c", Enabled: true, Priority: 5},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, clouds)
	chain := r.CloudChain()
	if len(chain) != 3 {
		t.Fatalf("len(CloudChain()) = %d, want 3", len(chain))
	}
	got := []string{chain[0].Name, chain[1].Name, chain[2].Name}
	want := []string{"high", "mid", "low"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CloudChain()[%d].Name = %q, want %q (order: %v)", i, got[i], want[i], got)
		}
	}
}

func TestCloudChainSkipsDisabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "off", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-a", Enabled: false, Priority: 10},
		{Name: "on", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-b", Enabled: true, Priority: 1},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, clouds)
	chain := r.CloudChain()
	if len(chain) != 1 || chain[0].Name != "on" {
		t.Fatalf("CloudChain() = %v, want only [on]", chain)
	}
}

func TestCloudChainUsesLiteLLMWhenEnabled(t *testing.T) {
	clouds := []config.CloudProvider{
		{Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-a", Enabled: true, Priority: 10},
	}
	r := New(config.RoutingConfig{}, []config.NodeConfig{}, clouds)
	r.SetLiteLLM(config.LiteLLMConfig{Enabled: true, URL: "http://localhost:4000", APIKey: "sk-litellm"})
	chain := r.CloudChain()
	if len(chain) != 1 {
		t.Fatalf("len(CloudChain()) = %d, want 1 (litellm only)", len(chain))
	}
	if chain[0].Name != "litellm" || chain[0].BaseURL != "http://localhost:4000" {
		t.Errorf("CloudChain()[0] = %+v, want synthetic litellm provider", chain[0])
	}
	if chain[0].APIKey != "sk-litellm" {
		t.Errorf("CloudChain()[0].APIKey = %q, want sk-litellm", chain[0].APIKey)
	}
	// Per-provider list is ignored entirely while LiteLLM is enabled - only
	// the synthetic entry appears, the "openai" provider above must not.
	for _, cp := range chain {
		if cp.Name == "openai" {
			t.Error("CloudChain() should not include per-provider entries while LiteLLM is enabled")
		}
	}
}
