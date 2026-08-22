// bench/ttft.go - Time-To-First-Token (TTFT) benchmark for marbor.
//
// Measures the wall-clock milliseconds from request send to the moment the
// first byte of the streaming NDJSON response is received.  Run it against
// two endpoints back-to-back to capture the warm vs cold delta.
//
// Build (from repo root, requires Go toolchain or Docker):
//
//	go build -o bench/ttft ./bench
//
// Or via Docker (no local toolchain needed):
//
//	docker run --rm -v "${PWD}:/app" -w /app -e GOFLAGS=-buildvcs=false \
//	  golang:1.25 go build -o bench/ttft ./bench
//
// Usage:
//
//	./bench/ttft --url http://localhost:11434 --model llama3.2:3b --n 10 \
//	             --api-key <key>
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

// ttftDrainCapBytes bounds the post-TTFT body drain so a server that keeps
// a stream open indefinitely can't block a sample forever.
const ttftDrainCapBytes = 8 << 20 // 8MB

func main() {
	url := flag.String("url", "http://localhost:11434", "Base URL of the endpoint to benchmark (marbor or direct Ollama)")
	model := flag.String("model", "llama3.2:3b", "Model name to request")
	n := flag.Int("n", 10, "Number of requests to send")
	apiKey := flag.String("api-key", "", "Bearer API key (required for marbor, omit for direct Ollama)")
	endpoint := flag.String("endpoint", "generate", "API endpoint: generate or chat")
	flag.Parse()

	if *n < 1 {
		fmt.Fprintln(os.Stderr, "error: --n must be >= 1")
		os.Exit(1)
	}

	fmt.Printf("Benchmarking TTFT: url=%s  model=%s  n=%d  endpoint=%s\n\n",
		*url, *model, *n, *endpoint)

	samples := make([]float64, 0, *n)
	client := &http.Client{
		// No timeout here - model cold-load on a slow GPU can take 60+ seconds.
		// The TTFT clock only starts when we send the request; the operator can
		// Ctrl-C if a run hangs unexpectedly.
	}

	fmt.Printf("  %-6s  %-10s  %s\n", "req", "ttft_ms", "status")
	fmt.Println("  ------  ----------  ------")

	for i := 1; i <= *n; i++ {
		ttft, status, err := measureTTFT(client, *url, *model, *endpoint, *apiKey)
		if err != nil {
			fmt.Printf("  %-6d  %-10s  ERROR: %v\n", i, "-", err)
			continue
		}
		samples = append(samples, ttft)
		fmt.Printf("  %-6d  %-10.1f  %s\n", i, ttft, status)
	}

	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "\nAll requests failed - no results to summarize.")
		os.Exit(1)
	}

	sort.Float64s(samples)
	fmt.Printf("\n  n=%d  p50=%.1f ms  p95=%.1f ms  min=%.1f ms  max=%.1f ms\n",
		len(samples),
		percentile(samples, 50),
		percentile(samples, 95),
		samples[0],
		samples[len(samples)-1],
	)
}

// measureTTFT sends one streaming request and returns milliseconds until the
// first response byte arrives, plus a short status string.
func measureTTFT(client *http.Client, baseURL, model, ep, apiKey string) (float64, string, error) {
	var path string
	var body []byte
	var err error

	switch ep {
	case "chat":
		path = "/api/chat"
		body, err = json.Marshal(map[string]interface{}{
			"model":  model,
			"stream": true,
			"messages": []map[string]string{
				{"role": "user", "content": "Say one word."},
			},
		})
	default:
		path = "/api/generate"
		body, err = json.Marshal(map[string]interface{}{
			"model":  model,
			"prompt": "Say one word.",
			"stream": true,
		})
	}
	if err != nil {
		return 0, "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Read until we have the first non-empty line (first NDJSON chunk).
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// First byte of real content received - record TTFT.
		ttft := float64(time.Since(start).Milliseconds())

		// Drain remainder synchronously (bounded) so the connection can be
		// reused. An unbounded goroutine here would race the deferred
		// Close() above - Close() mid-read can error the drain, and for a
		// server that keeps the stream open the goroutine (and its
		// connection) would never return, leaking across many samples.
		io.Copy(io.Discard, io.LimitReader(resp.Body, ttftDrainCapBytes)) //nolint:errcheck

		status := fmt.Sprintf("HTTP %d", resp.StatusCode)
		return ttft, status, nil
	}

	if err := scanner.Err(); err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	return 0, "", fmt.Errorf("empty response body (HTTP %d)", resp.StatusCode)
}

// percentile returns the p-th percentile of a pre-sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p / 100.0 * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
