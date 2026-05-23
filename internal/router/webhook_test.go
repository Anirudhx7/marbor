package router

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

func newTestRouter(nodes []config.NodeConfig, webhook config.WebhookConfig) *Router {
	r := New(
		config.RoutingConfig{
			Strategy:       "warm-first",
			PollIntervalMs: 2000,
			Fallback:       "least-connections",
		},
		nodes,
		nil,
	)
	r.SetWebhookConfig(webhook)
	return r
}

func TestWebhookFiredOnNodeDown(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]string
		gotSig   string
	)

	secret := "test-secret-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("invalid JSON body: %v", err)
			return
		}
		mu.Lock()
		received = append(received, payload)
		gotSig = r.Header.Get("X-Ollama-Mesh-Signature")
		mu.Unlock()

		// Verify HMAC signature.
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := fmt.Sprintf("sha256=%s", hex.EncodeToString(mac.Sum(nil)))
		if gotSig != want {
			t.Errorf("signature = %q, want %q", gotSig, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRouter(
		[]config.NodeConfig{{Name: "gpu-0", URL: "http://localhost:19999"}},
		config.WebhookConfig{Enabled: true, URL: srv.URL, Secret: secret},
	)

	// Manually drive markFailure 3 times to trigger the transition.
	node := r.nodes[0]
	r.markFailure(node)
	r.markFailure(node)
	r.markFailure(node)

	// Give the goroutine time to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) == 0 {
		t.Fatal("webhook not called")
	}
	p := received[0]
	if p["event"] != "node_down" {
		t.Errorf("event = %q, want node_down", p["event"])
	}
	if p["node"] != "gpu-0" {
		t.Errorf("node = %q, want gpu-0", p["node"])
	}
	if p["time"] == "" {
		t.Error("time field missing")
	}
}

func TestWebhookFiredOnNodeRecovery(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRouter(
		[]config.NodeConfig{{Name: "gpu-1", URL: "http://localhost:19998"}},
		config.WebhookConfig{Enabled: true, URL: srv.URL},
	)

	node := r.nodes[0]

	// Drive node to unhealthy state.
	r.markFailure(node)
	r.markFailure(node)
	r.markFailure(node)

	// Wait for node_down webhook.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Simulate recovery: set Healthy back to false so pollNode logic fires node_up.
	// We call the recovery path directly by manipulating prevHealthy.
	r.mu.Lock()
	r.prevHealthy["gpu-1"] = false
	r.mu.Unlock()
	node.mu.Lock()
	node.Healthy = false
	node.mu.Unlock()

	// Call the recovery section directly (same code path as pollNode success).
	r.mu.Lock()
	prev, seen := r.prevHealthy["gpu-1"]
	if seen && !prev {
		r.prevHealthy["gpu-1"] = true
		r.mu.Unlock()
		r.fireWebhook("node_up", "gpu-1", node.URL)
	} else {
		r.mu.Unlock()
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) < 2 {
		t.Fatalf("expected 2 webhook calls (node_down + node_up), got %d", len(received))
	}
	events := make([]string, len(received))
	for i, p := range received {
		events[i] = p["event"]
	}
	hasDown := false
	hasUp := false
	for _, e := range events {
		if e == "node_down" {
			hasDown = true
		}
		if e == "node_up" {
			hasUp = true
		}
	}
	if !hasDown {
		t.Error("missing node_down event")
	}
	if !hasUp {
		t.Error("missing node_up event")
	}
}

func TestWebhookNotFiredWhenDisabled(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRouter(
		[]config.NodeConfig{{Name: "gpu-2", URL: "http://localhost:19997"}},
		config.WebhookConfig{Enabled: false, URL: srv.URL},
	)

	node := r.nodes[0]
	r.markFailure(node)
	r.markFailure(node)
	r.markFailure(node)

	// Give goroutine time if it were to fire.
	time.Sleep(200 * time.Millisecond)

	if called {
		t.Error("webhook called but should not be when disabled")
	}
}
