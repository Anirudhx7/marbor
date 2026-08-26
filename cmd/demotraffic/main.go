// cmd/demotraffic/main.go - Sends demo traffic through the marbor proxy to populate dashboard analytics.
// Configurable via env vars: PROXY_URL, API_KEY, REQUEST_COUNT
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// requestSpec defines one traffic pattern to send.
type requestSpec struct {
	model  string
	prompt string
}

var trafficMix = []requestSpec{
	{model: "llama3.2:3b", prompt: "What is machine learning?"},
	{model: "mistral:7b", prompt: "Explain the difference between SQL and NoSQL."},
	{model: "qwen2.5:7b", prompt: "Write a haiku about distributed systems."},
	{model: "llama3.2:3b", prompt: "What is a Kubernetes pod?"},
	{model: "mistral:7b", prompt: "How does TLS handshake work?"},
	{model: "qwen2.5:7b", prompt: "Explain gradient descent in simple terms."},
	{model: "llama3.2:3b", prompt: "What is the CAP theorem?"},
	{model: "mistral:7b", prompt: "Describe the observer design pattern."},
	{model: "qwen2.5:7b", prompt: "What is a Bloom filter?"},
	{model: "llama3.2:3b", prompt: "How does consistent hashing work?"},
	{model: "mistral:7b", prompt: "What is a circuit breaker pattern?"},
	{model: "qwen2.5:7b", prompt: "Explain RAFT consensus algorithm briefly."},
	{model: "llama3.2:3b", prompt: "What is VRAM and why does it matter for LLMs?"},
	{model: "mistral:7b", prompt: "Compare TCP and UDP."},
	{model: "qwen2.5:7b", prompt: "What is a goroutine?"},
	{model: "llama3.2:3b", prompt: "Explain pub/sub messaging."},
	{model: "mistral:7b", prompt: "What is zero-trust networking?"},
	{model: "qwen2.5:7b", prompt: "How does a reverse proxy work?"},
	{model: "llama3.2:3b", prompt: "What is infrastructure as code?"},
	{model: "mistral:7b", prompt: "Explain warm vs cold model loading."},
}

func sendRequest(client *http.Client, proxyURL, apiKey string, spec requestSpec, idx, total int) error {
	body, _ := json.Marshal(generateRequest{
		Model:  spec.model,
		Prompt: spec.prompt,
		Stream: true,
	})

	req, err := http.NewRequest(http.MethodPost, proxyURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Marks this as synthetic dashboard-seeding traffic (R1/P165), not
	// genuine usage - without it, a request sent by this dev tool is
	// byte-for-byte indistinguishable from real traffic in dashboards,
	// analytics, and the audit log if pointed at a live instance.
	req.Header.Set("X-Marbor-Demo-Traffic", "1")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Drain the streaming response and capture final eval_count
	var totalTokens int
	var tokenCountMissing bool
	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var chunk map[string]interface{}
		if err := decoder.Decode(&chunk); err != nil {
			break
		}
		if done, _ := chunk["done"].(bool); done {
			if ec, ok := chunk["eval_count"].(float64); ok {
				totalTokens = int(ec)
			} else {
				tokenCountMissing = true
			}
		}
	}

	elapsed := time.Since(start)
	if tokenCountMissing {
		fmt.Printf("  [%2d/%-2d] %-15s  n/a tokens (eval_count missing)  %5dms  HTTP %d\n",
			idx+1, total, spec.model, elapsed.Milliseconds(), resp.StatusCode)
	} else {
		fmt.Printf("  [%2d/%-2d] %-15s  %3d tokens  %5dms  HTTP %d\n",
			idx+1, total, spec.model, totalTokens, elapsed.Milliseconds(), resp.StatusCode)
	}
	return nil
}

// isLocalhostURL reports whether rawURL's host resolves to a well-known
// loopback name/address, without a DNS lookup (a bare string comparison
// against the handful of forms an operator would actually type for
// localhost).
func isLocalhostURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func main() {
	proxyURL := strings.TrimRight(envOrDefault("PROXY_URL", "http://localhost:11434"), "/")
	apiKey := envOrDefault("API_KEY", "demo-api-key")

	// R1/P165: this tool exists to seed a local dev/demo instance's
	// dashboard with plausible-looking traffic - pointed at a live
	// instance, it would populate real dashboards/analytics/audit with
	// synthetic requests indistinguishable from genuine usage. Require an
	// explicit, deliberate opt-in before it will target anything else.
	if !isLocalhostURL(proxyURL) && os.Getenv("ALLOW_NON_LOCALHOST") != "1" {
		fmt.Fprintf(os.Stderr, "demotraffic: PROXY_URL %q does not resolve to localhost - refusing to send synthetic traffic to it.\n"+
			"Set ALLOW_NON_LOCALHOST=1 to override if you really mean to seed a non-local instance's dashboard.\n", proxyURL)
		os.Exit(1)
	}
	count, err := strconv.Atoi(envOrDefault("REQUEST_COUNT", "20"))
	if err != nil || count <= 0 {
		count = 20
	}
	fmt.Printf("Sending %d requests to %s\n", count, proxyURL)
	fmt.Println(strings.Repeat("-", 60))

	client := &http.Client{Timeout: 30 * time.Second}

	var failures int
	for i := 0; i < count; i++ {
		spec := trafficMix[i%len(trafficMix)]
		if err := sendRequest(client, proxyURL, apiKey, spec, i, count); err != nil {
			fmt.Printf("  [%2d/%2d] ERROR %s: %v\n", i+1, count, spec.model, err)
			failures++
		}
		// Small pause so the request log shows spread timestamps in the dashboard
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("Done: %d succeeded, %d failed\n", count-failures, failures)

	if failures > 0 {
		os.Exit(1)
	}
}
