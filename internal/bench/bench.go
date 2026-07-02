// Package bench implements the "ollama-mesh bench" subcommand.
//
// It measures cold vs warm Time-To-First-Token (TTFT) through the mesh proxy
// using the OpenAI-compatible /v1/chat/completions streaming endpoint.
// The --target flag must point at the mesh proxy port (e.g.
// http://localhost:11435), NOT at an Ollama backend directly — we measure
// what the mesh adds, not raw Ollama speed.
//
// Usage:
//
//	ollama-mesh bench --target http://localhost:11435 [--model llama3:8b] [--json] [--key <api-key>] [--timeout 120s]
package bench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Result holds the measured cold and warm TTFT values.
type Result struct {
	Model          string  `json:"model"`
	ColdMs         int64   `json:"cold_ms"`
	WarmMs         int64   `json:"warm_ms"`
	ImprovementX   float64 `json:"improvement_x"`
	ImprovementPct float64 `json:"improvement_pct"`
}

// Run is the entry-point for the "bench" subcommand.  args is os.Args[2:].
func Run(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ollama-mesh bench [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Measures cold vs warm Time-To-First-Token (TTFT) through the mesh proxy.\n")
		fmt.Fprintf(os.Stderr, "--target must point at the mesh proxy port, not an Ollama backend directly.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	target  := fs.String("target", "http://localhost:11435", "Mesh proxy base URL (not the Ollama backend)")
	model   := fs.String("model", "", "Model to benchmark (auto-detected from /v1/models if omitted)")
	apiKey  := fs.String("key", "", "Bearer API key (required if auth is enabled on the mesh)")
	jsonOut := fs.Bool("json", false, "Emit JSON output instead of the human-readable table")
	timeout := fs.Duration("timeout", 300*time.Second, "Per-request timeout (cold load can take minutes on a large model)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	client := &http.Client{Timeout: *timeout}

	// ── 1. Resolve model ────────────────────────────────────────────────────
	resolvedModel := *model
	if resolvedModel == "" {
		var err error
		resolvedModel, err = detectModel(client, *target, *apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench: cannot detect model from %s/v1/models: %v\n", *target, err)
			fmt.Fprintf(os.Stderr, "bench: use --model to specify one explicitly\n")
			os.Exit(1)
		}
		if !*jsonOut {
			fmt.Printf("bench: auto-detected model %q from %s/v1/models\n\n", resolvedModel, *target)
		}
	}

	// ── 2. Cold TTFT ────────────────────────────────────────────────────────
	if !*jsonOut {
		fmt.Printf("Sending cold request (model loading from disk)...\n")
	}
	coldStart := time.Now()
	coldMs, err := measureTTFT(client, *target, resolvedModel, *apiKey)
	_ = coldStart
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: cold request failed: %v\n", err)
		os.Exit(1)
	}

	// ── 3. Warm TTFT (immediate repeat — model now in VRAM) ─────────────────
	if !*jsonOut {
		fmt.Printf("Sending warm request (model in VRAM)...\n\n")
	}
	warmMs, err := measureTTFT(client, *target, resolvedModel, *apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: warm request failed: %v\n", err)
		os.Exit(1)
	}

	// ── 4. Compute improvement ───────────────────────────────────────────────
	var improvX float64
	var improvPct float64
	if warmMs > 0 {
		improvX = float64(coldMs) / float64(warmMs)
	}
	if coldMs > 0 {
		improvPct = (1.0 - float64(warmMs)/float64(coldMs)) * 100.0
	}

	res := Result{
		Model:          resolvedModel,
		ColdMs:         coldMs,
		WarmMs:         warmMs,
		ImprovementX:   roundTo1(improvX),
		ImprovementPct: roundTo1(improvPct),
	}

	// ── 5. Output ────────────────────────────────────────────────────────────
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "bench: json encode: %v\n", err)
			os.Exit(1)
		}
		return
	}

	printTable(res)
}

// printTable prints the human-readable results table.
func printTable(r Result) {
	improvStr := fmt.Sprintf("%.0fx faster (%.1f%%)", r.ImprovementX, r.ImprovementPct)

	// Column widths for alignment.
	labelW := 13
	fmt.Printf("\n")
	fmt.Printf("  %-*s %s\n", labelW, "Model:", r.Model)
	fmt.Printf("  %-*s %s\n", labelW, "Cold TTFT:", fmtMs(r.ColdMs))
	fmt.Printf("  %-*s %s\n", labelW, "Warm TTFT:", fmtMs(r.WarmMs))
	fmt.Printf("  %-*s %s\n", labelW, "Improvement:", improvStr)
	fmt.Printf("\n")
}

// fmtMs formats a millisecond count with comma-thousands separation,
// e.g. 22340 → "22,340ms".
func fmtMs(ms int64) string {
	s := fmt.Sprintf("%d", ms)
	// Insert commas every 3 digits from the right.
	n := len(s)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteRune(',')
		}
		b.WriteRune(ch)
	}
	return b.String() + "ms"
}

// roundTo1 rounds a float64 to one decimal place.
func roundTo1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// detectModel calls GET /v1/models on the mesh and returns the first model ID.
func detectModel(client *http.Client, target, apiKey string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, target+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", fmt.Errorf("decode /v1/models: %w", err)
	}
	if len(list.Data) == 0 {
		return "", fmt.Errorf("no models available on the mesh (is any Ollama node healthy?)")
	}
	return list.Data[0].ID, nil
}

// measureTTFT sends a single streaming /v1/chat/completions request through
// the mesh and returns the milliseconds until the first non-empty token
// arrives in the SSE stream.
//
// Using the OpenAI-compatible endpoint ensures the request travels through
// the full mesh routing stack (proxy → router → backend), not a direct hop
// to an Ollama node.
func measureTTFT(client *http.Client, target, model, apiKey string) (int64, error) {
	payload := map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "Say one word."},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, target+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Parse SSE stream: lines are "data: <json>" or "data: [DONE]".
	// Record TTFT on the first chunk that carries a non-empty token.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			ttft := time.Since(start).Milliseconds()
			// Drain the rest so the connection is reusable for the warm request.
			go io.Copy(io.Discard, resp.Body) //nolint:errcheck
			return ttft, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read stream: %w", err)
	}
	return 0, fmt.Errorf("no tokens received — model may have failed to load, or the mesh has no healthy nodes")
}
