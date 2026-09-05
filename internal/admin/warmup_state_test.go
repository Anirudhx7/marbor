package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

// TestHandleNodes_WarmupStateSurfacesSuppression verifies GET /admin/nodes
// exposes a manually-unloaded model as a "suppressed" warmupState entry with
// a reason - the whole point being that this state previously had zero
// admin/UI surface (only a raw internal bool an operator could never see). It must never leak the raw warmupSuppressed map/bool shape.
func TestHandleNodes_WarmupStateSurfacesSuppression(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://127.0.0.1:19999"}}, nil)
	r.RecordManualUnload("gpu-0", "llama3.2")
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var nodes []nodeResp
	if err := json.NewDecoder(res.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if len(nodes[0].WarmupState) != 1 {
		t.Fatalf("expected 1 warmupState entry, got %d: %+v", len(nodes[0].WarmupState), nodes[0].WarmupState)
	}
	entry := nodes[0].WarmupState[0]
	if entry.Model != "llama3.2" {
		t.Errorf("Model = %q, want llama3.2", entry.Model)
	}
	if entry.State != "suppressed" {
		t.Errorf("State = %q, want suppressed", entry.State)
	}
	if entry.Reason != "manual_unload" {
		t.Errorf("Reason = %q, want manual_unload", entry.Reason)
	}
	if entry.Since == "" {
		t.Error("Since is empty, want an RFC3339 timestamp")
	}
}

// TestHandleNodes_WarmupStateEmptyWhenNothingSuppressed verifies a node with
// no suppressed models reports an empty/absent warmupState, not a spurious
// entry.
func TestHandleNodes_WarmupStateEmptyWhenNothingSuppressed(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-0", URL: "http://127.0.0.1:19999"}}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var nodes []nodeResp
	if err := json.NewDecoder(w.Result().Body).Decode(&nodes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if len(nodes[0].WarmupState) != 0 {
		t.Errorf("expected no warmupState entries, got %+v", nodes[0].WarmupState)
	}
}
