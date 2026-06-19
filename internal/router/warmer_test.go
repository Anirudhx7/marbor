package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func TestNodesForEntry(t *testing.T) {
	nodes := []*NodeState{
		{Name: "gpu-1"},
		{Name: "gpu-2"},
		{Name: "gpu-3"},
	}

	// empty allow list = all nodes
	got := nodesForEntry(nodes, nil)
	if len(got) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(got))
	}

	// specific subset
	got = nodesForEntry(nodes, []string{"gpu-1", "gpu-3"})
	if len(got) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(got))
	}
	if got[0].Name != "gpu-1" || got[1].Name != "gpu-3" {
		t.Errorf("unexpected nodes: %v", got)
	}

	// unknown name = no match
	got = nodesForEntry(nodes, []string{"gpu-99"})
	if len(got) != 0 {
		t.Fatalf("expected 0 nodes, got %d", len(got))
	}
}

func TestPingNodeUnhealthySkipped(t *testing.T) {
	r := &Router{client: &http.Client{Timeout: 1 * time.Second}}
	n := &NodeState{Name: "dead", URL: "http://127.0.0.1:19999", Healthy: false}
	err := r.pingNode(context.Background(), n, "llama3.2", "10m")
	if err == nil {
		t.Fatal("expected error for unhealthy node, got nil")
	}
}

func TestPingNodeSuccess(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	router := &Router{client: &http.Client{Timeout: 5 * time.Second}}
	n := &NodeState{Name: "test", URL: srv.URL, Healthy: true}
	err := router.pingNode(context.Background(), n, "llama3.2", "10m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
}

func TestPingWarmupModelsDisabled(t *testing.T) {
	// disabled warmup = no panics, no HTTP calls
	r := &Router{
		client:    &http.Client{Timeout: time.Second},
		warmupCfg: config.WarmupConfig{Enabled: false},
	}
	r.pingWarmupModels(context.Background()) // should return immediately
}

func TestPingWarmupModelsAllNodes(t *testing.T) {
	calls := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Host
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		client: &http.Client{Timeout: 5 * time.Second},
		nodes:  []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		warmupCfg: config.WarmupConfig{
			Enabled:   true,
			KeepAlive: "5m",
			Models:    []config.WarmupEntry{{Model: "llama3.2"}},
		},
	}
	r.pingWarmupModels(context.Background())

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup ping never fired")
	}
}
