package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func psServer(t *testing.T, models []map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
	}))
}

// TestVRAMSourceAPI: a node with loaded models but no declared total and no
// local nvidia-smi (CI) reports real used-VRAM from /api/ps and source "api".
func TestVRAMSourceAPI(t *testing.T) {
	srv := psServer(t, []map[string]interface{}{
		{"name": "llama3.2:3b", "size_vram": 2097152000},
	})
	defer srv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "n", URL: srv.URL},
	}, nil)
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].VRAMSource != "api" {
		t.Errorf("VRAMSource = %q, want \"api\"", r.nodes[0].VRAMSource)
	}
	if r.nodes[0].VRAMUsedMB != 2097152000/(1024*1024) {
		t.Errorf("VRAMUsedMB = %d, want %d", r.nodes[0].VRAMUsedMB, 2097152000/(1024*1024))
	}
	if r.nodes[0].VRAMTotalMB != 0 {
		t.Errorf("VRAMTotalMB = %d, want 0 (capacity unknown)", r.nodes[0].VRAMTotalMB)
	}
}

// TestVRAMSourceNone: a node with no loaded models and no declared total has
// nothing to report and source "none".
func TestVRAMSourceNone(t *testing.T) {
	srv := psServer(t, []map[string]interface{}{})
	defer srv.Close()

	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "n", URL: srv.URL},
	}, nil)
	r.pollNode(r.nodes[0])

	r.nodes[0].mu.RLock()
	defer r.nodes[0].mu.RUnlock()
	if r.nodes[0].VRAMSource != "none" {
		t.Errorf("VRAMSource = %q, want \"none\"", r.nodes[0].VRAMSource)
	}
	if r.nodes[0].VRAMUsedMB != 0 {
		t.Errorf("VRAMUsedMB = %d, want 0", r.nodes[0].VRAMUsedMB)
	}
}
