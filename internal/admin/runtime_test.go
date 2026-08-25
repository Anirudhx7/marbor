package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
)

func TestHandleNodes_RuntimeField_VllmNode(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "vllm-node", URL: "http://localhost:8000", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+"")
	rec := httptest.NewRecorder()
	s.handleNodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var nodes []map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	got, ok := nodes[0]["runtime"].(string)
	if !ok {
		t.Fatalf("runtime field missing or not a string")
	}
	if got != "vllm" {
		t.Errorf("runtime = %q, want %q", got, "vllm")
	}
}

func TestHandleNodes_RuntimeField_DefaultOllama(t *testing.T) {
	// Runtime "" is normalised to "ollama" by config.Validate, but router.New
	// also copies NodeConfig.Runtime directly into NodeState.Runtime. Use an
	// explicit "ollama" value (what Validate would set) to mirror production.
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "ollama-node", URL: "http://localhost:11434", Runtime: "ollama"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+"")
	rec := httptest.NewRecorder()
	s.handleNodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var nodes []map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	got, ok := nodes[0]["runtime"].(string)
	if !ok {
		t.Fatalf("runtime field missing or not a string")
	}
	if got != "ollama" {
		t.Errorf("runtime = %q, want %q", got, "ollama")
	}
}

// TestHandleNodes_AgentStaleOmittedWhenFalse verifies agentStale is absent
// (omitempty) for a node whose host has no agent configured - the common
// agentless-fleet shape. The true case's transition logic is covered at the
// router level (TestAgentStale_DistinguishesConfiguredDownFromNeverConfigured);
// this pins the admin API wire contract: the field appears only when set.
func TestHandleNodes_AgentStaleOmittedWhenFalse(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "plain-node", URL: "http://localhost:11434", Runtime: "ollama"},
	}, nil)
	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+"")
	rec := httptest.NewRecorder()
	s.handleNodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var nodes []map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if v, present := nodes[0]["agentStale"]; present {
		t.Errorf("agentStale = %v for an agentless node, want the key omitted entirely", v)
	}
}
