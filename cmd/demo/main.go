// cmd/demo/main.go - standalone demo binary for ollama-mesh
//
// Starts 2 in-process mock Ollama nodes, wires up the full mesh stack
// (router, proxy, admin), sends 30 real requests through the proxy so
// the dashboard shows real token counts and savings, then keeps running
// until Ctrl-C.
//
// Usage:
//
//	go run ./cmd/demo
//
// Ports used (to avoid clashing with a real running instance):
//
//	:11437  proxy (Ollama-compatible endpoint)
//	:8082   admin dashboard
//
// No config.yaml required. No real Ollama required.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/proxy"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// ----------------------------------------------------------------------------
// Mock Ollama server
// ----------------------------------------------------------------------------

// mockOllamaServer runs an in-process HTTP server that mimics Ollama endpoints.
// warmModel is the model name that will appear in /api/ps (pre-loaded in VRAM).
type mockOllamaServer struct {
	warmModel string
	server    *http.Server
	addr      string
}

func newMockOllamaServer(warmModel string) (*mockOllamaServer, error) {
	// Pick a random free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	m := &mockOllamaServer{
		warmModel: warmModel,
		addr:      "http://" + ln.Addr().String(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ps", m.handlePS)
	mux.HandleFunc("/api/chat", m.handleChat)
	mux.HandleFunc("/api/generate", m.handleGenerate)
	// Health check used by the router poller.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	m.server = &http.Server{Handler: mux}
	go func() {
		if err := m.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("mock server %s: %v", m.addr, err)
		}
	}()
	return m, nil
}

func (m *mockOllamaServer) shutdown(ctx context.Context) {
	_ = m.server.Shutdown(ctx)
}

// handlePS responds to GET /api/ps with the warm model pre-loaded.
func (m *mockOllamaServer) handlePS(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		Name      string   `json:"name"`
		Size      int64    `json:"size"`
		Digest    string   `json:"digest"`
		Details   struct{} `json:"details"`
		ExpiresAt string   `json:"expires_at"`
		SizeVRAM  int64    `json:"size_vram"`
	}
	resp := struct {
		Models []modelEntry `json:"models"`
	}{
		Models: []modelEntry{
			{
				Name:      m.warmModel,
				Size:      4294967296,
				Digest:    "abc123def456",
				ExpiresAt: "2099-01-01T00:00:00Z",
				SizeVRAM:  4294967296,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// writeNDJSON sends a realistic two-chunk NDJSON streaming response.
// Chunk 1: content token. Chunk 2: done=true with eval_count/prompt_eval_count.
func writeNDJSON(w http.ResponseWriter, model, content string, evalCount, promptEvalCount int) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)

	// Chunk 1: partial content.
	_ = enc.Encode(map[string]interface{}{
		"model":      model,
		"created_at": "2026-01-01T00:00:00Z",
		"message": map[string]string{
			"role":    "assistant",
			"content": content,
		},
		"done": false,
	})
	if flusher != nil {
		flusher.Flush()
	}

	// Chunk 2: done with token counts so admin.Server can parse savings.
	_ = enc.Encode(map[string]interface{}{
		"model":             model,
		"created_at":        "2026-01-01T00:00:00Z",
		"message":           map[string]string{"role": "assistant", "content": ""},
		"done":              true,
		"eval_count":        evalCount,
		"prompt_eval_count": promptEvalCount,
		"total_duration":    500000000,
	})
	if flusher != nil {
		flusher.Flush()
	}
}

func (m *mockOllamaServer) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	model := req.Model
	if model == "" {
		model = m.warmModel
	}
	writeNDJSON(w, model, "Hello! I'm a demo response from ollama-mesh.", 42, 18)
}

func (m *mockOllamaServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	model := req.Model
	if model == "" {
		model = m.warmModel
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]interface{}{
		"model":      model,
		"created_at": "2026-01-01T00:00:00Z",
		"response":   "This is a demo response from the ollama-mesh proxy.",
		"done":       false,
	})
	if flusher != nil {
		flusher.Flush()
	}
	_ = enc.Encode(map[string]interface{}{
		"model":             model,
		"created_at":        "2026-01-01T00:00:00Z",
		"response":          "",
		"done":              true,
		"eval_count":        35,
		"prompt_eval_count": 12,
		"total_duration":    400000000,
	})
	if flusher != nil {
		flusher.Flush()
	}
}

// ----------------------------------------------------------------------------
// Demo config builder
// ----------------------------------------------------------------------------

func buildDemoConfig(node1URL, node2URL string, apiKey string) config.Config {
	return config.Config{
		Proxy: config.ProxyConfig{
			Port: 11437,
		},
		Auth: config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys: []config.KeyConfig{
				{
					Name:      "demo",
					Key:       apiKey,
					RateLimit: 1000,
				},
			},
		},
		Nodes: []config.NodeConfig{
			{Name: "node-1", URL: node1URL, GPUModel: "Demo GPU (mock)"},
			{Name: "node-2", URL: node2URL, GPUModel: "Demo GPU (mock)"},
		},
		Routing: config.RoutingConfig{
			Strategy:       "warm-first",
			PollIntervalMs: 2000,
			Fallback:       "least-connections",
		},
		Metrics: config.MetricsConfig{
			Enabled: false,
			Port:    9092,
		},
		Savings: config.SavingsConfig{
			ReferenceCostPer1K: 0.002,
		},
	}
}

// ----------------------------------------------------------------------------
// Traffic generator
// ----------------------------------------------------------------------------

// waitForProxyReady polls the proxy's base URL until it responds (any status
// counts - we only care that the listener is accepting connections) or the
// deadline elapses.
func waitForProxyReady(client *http.Client, proxyURL string, deadline time.Duration) {
	start := time.Now()
	for time.Since(start) < deadline {
		resp, err := client.Get(proxyURL + "/")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Println("demo: proxy readiness poll timed out, proceeding anyway")
}

// sendDemoTraffic sends 30 requests through the proxy to populate the dashboard.
// 20 warm requests (llama3:8b - the warm model on both nodes)
// 5 cold requests (mistral:7b - not warm, routes via fallback)
// 5 generate requests (llama3:8b again, different endpoint)
func sendDemoTraffic(proxyURL, apiKey string) {
	client := &http.Client{Timeout: 10 * time.Second}

	send := func(endpoint, model string, i int) {
		var bodyBytes []byte
		if strings.Contains(endpoint, "chat") {
			payload := map[string]interface{}{
				"model": model,
				"messages": []map[string]string{
					{"role": "user", "content": fmt.Sprintf("Demo request %d: say hello", i)},
				},
				"stream": true,
			}
			bodyBytes, _ = json.Marshal(payload)
		} else {
			payload := map[string]interface{}{
				"model":  model,
				"prompt": fmt.Sprintf("Demo prompt %d: describe yourself briefly", i),
				"stream": true,
			}
			bodyBytes, _ = json.Marshal(payload)
		}

		req, err := http.NewRequest(http.MethodPost, proxyURL+endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			log.Printf("demo traffic: create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("demo traffic: %s %s: %v", endpoint, model, err)
			return
		}
		// Drain body so streaming completes and token counts are captured.
		buf := make([]byte, 4096)
		for {
			_, err := resp.Body.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("demo traffic: %s %s: truncated stream: %v", endpoint, model, err)
				}
				break
			}
		}
		resp.Body.Close()
	}

	// Wait for the proxy to be ready by polling it instead of a fixed sleep -
	// the HTTP server is started in a separate goroutine with no readiness signal.
	waitForProxyReady(client, proxyURL, 5*time.Second)

	log.Println("demo: sending 30 requests through proxy...")

	// 20 warm chat requests (llama3:8b - pre-loaded in VRAM on both nodes)
	for i := 0; i < 20; i++ {
		send("/api/chat", "llama3:8b", i+1)
		time.Sleep(50 * time.Millisecond)
	}

	// 5 cold chat requests (mistral:7b - not warm, falls back to least-conns)
	for i := 0; i < 5; i++ {
		send("/api/chat", "mistral:7b", i+21)
		time.Sleep(50 * time.Millisecond)
	}

	// 5 generate requests (llama3:8b again via /api/generate)
	for i := 0; i < 5; i++ {
		send("/api/generate", "llama3:8b", i+26)
		time.Sleep(50 * time.Millisecond)
	}

	log.Println("demo: 30 requests complete - dashboard is populated")
}

// ----------------------------------------------------------------------------
// Main
// ----------------------------------------------------------------------------

func main() {
	log.SetPrefix("[ollama-mesh demo] ")

	// Generate a stable API key for the demo session.
	apiKey := "demo-key-ollama-mesh-2026"

	// Start 2 mock Ollama nodes. Both have llama3:8b warm in VRAM.
	log.Println("Starting mock Ollama node 1 (warm: llama3:8b)...")
	node1, err := newMockOllamaServer("llama3:8b")
	if err != nil {
		log.Fatalf("node1: %v", err)
	}

	log.Println("Starting mock Ollama node 2 (warm: llama3:8b)...")
	node2, err := newMockOllamaServer("llama3:8b")
	if err != nil {
		log.Fatalf("node2: %v", err)
	}

	log.Printf("node-1: %s", node1.addr)
	log.Printf("node-2: %s", node2.addr)

	// Build in-memory config (no config.yaml needed).
	cfg := buildDemoConfig(node1.addr, node2.addr, apiKey)

	// Wire up the mesh stack.
	authMw := auth.NewMiddleware(cfg.Auth)

	r := router.New(cfg.Routing, cfg.Nodes, cfg.CloudProviders)
	r.SetDockerConfig(cfg.Docker)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	// Audit log: disabled for demo.
	auditLog := audit.New(store.NopStore{}, false)
	defer auditLog.Close()

	adminSrv := admin.NewServer(r, authMw, cfg)
	adminSrv.SetAuditLogger(auditLog)
	adminSrv.SetDemoMode(true) // no store attached; admin/admin login without a DB

	proxyHandler := proxy.NewHandler(r, adminSrv, auditLog)
	wrapped := authMw.Handler(proxyHandler)

	proxyAddr := fmt.Sprintf(":%d", cfg.Proxy.Port)
	proxySrv := &http.Server{
		Addr:         proxyAddr,
		Handler:      wrapped,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	adminHttpSrv := &http.Server{
		Addr:         ":8082",
		Handler:      adminSrv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("proxy server: %v", err)
		}
	}()

	go func() {
		if err := adminHttpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("admin server: %v", err)
		}
	}()

	// Send demo traffic in background after a short startup delay.
	go sendDemoTraffic(fmt.Sprintf("http://localhost%s", proxyAddr), apiKey)

	// Print banner.
	fmt.Println()
	fmt.Println("================================================================")
	fmt.Println("  ollama-mesh demo")
	fmt.Println("================================================================")
	fmt.Println()
	fmt.Println("  2 mock Ollama nodes started (in-process, no real GPU needed)")
	fmt.Println("  Sending 30 real requests through the proxy...")
	fmt.Println()
	fmt.Printf("  Proxy endpoint:  http://localhost%s\n", proxyAddr)
	fmt.Printf("  API key:         %s\n", apiKey)
	fmt.Println()
	fmt.Println("  Open dashboard:  http://localhost:8082")
	fmt.Println("  Dashboard login: admin / admin")
	fmt.Println()
	fmt.Println("  Press Ctrl-C to stop.")
	fmt.Println("================================================================")
	fmt.Println()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("Shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	cancel()
	_ = proxySrv.Shutdown(shutCtx)
	_ = adminHttpSrv.Shutdown(shutCtx)
	node1.shutdown(shutCtx)
	node2.shutdown(shutCtx)
	log.Println("Done.")
}
