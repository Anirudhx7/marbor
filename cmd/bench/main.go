// bench measures warm-vs-cold first-token latency through a marbor or
// Ollama endpoint. Run it while screen-recording to produce the side-by-side
// demo that proves warm routing is worth it.
//
// Usage:
//
//	go run ./cmd/bench [flags]
//
// Flags:
//
//	-endpoint   http://localhost:11434   proxy or Ollama base URL
//	-model      llama3                   model to test (must be pulled)
//	-key        ""                       API key if auth is enabled
//	-prompt     "Say exactly three words." prompt sent each request
//	-runs       3                        warm runs after the cold baseline
//	-timeout    120s                     per-request timeout
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
	"strings"
	"time"
)

func main() {
	endpoint := flag.String("endpoint", "http://localhost:11434", "Ollama or marbor base URL")
	model := flag.String("model", "llama3", "model name (must be pulled on the target node)")
	apiKey := flag.String("key", "", "Bearer token if auth is enabled")
	prompt := flag.String("prompt", "Say exactly three words.", "prompt sent on each request")
	runs := flag.Int("runs", 3, "number of warm-run repetitions after the cold baseline")
	timeout := flag.Duration("timeout", 120*time.Second, "per-request deadline")
	flag.Parse()

	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "error: -runs must be >= 1")
		os.Exit(1)
	}

	client := &http.Client{Timeout: *timeout}

	fmt.Println()
	fmt.Println("=== marbor warm-vs-cold benchmark ===")
	fmt.Printf("endpoint : %s\n", *endpoint)
	fmt.Printf("model    : %s\n", *model)
	fmt.Printf("prompt   : %q\n", *prompt)
	fmt.Println()

	// ── Step 1: evict model so we start cold ──────────────────────────────────
	fmt.Print("Evicting model from VRAM ... ")
	if err := evict(client, *endpoint, *model, *apiKey); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: evict failed (%v) - proceeding anyway\n", err)
	} else {
		fmt.Println("done")
	}
	fmt.Println()

	// ── Step 2: cold baseline ─────────────────────────────────────────────────
	fmt.Printf("COLD (model loading from disk)\n")
	coldFirst, _, err := measure(client, *endpoint, *model, *prompt, *apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// ── Step 3: warm runs ─────────────────────────────────────────────────────
	fmt.Printf("WARM (model already in VRAM) - %d run(s)\n", *runs)
	var warmSamples []time.Duration
	for i := 1; i <= *runs; i++ {
		ft, _, err := measure(client, *endpoint, *model, *prompt, *apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  run %d error: %v\n", i, err)
			continue
		}
		warmSamples = append(warmSamples, ft)
	}
	fmt.Println()

	// Scale progress bar to the actual max duration so cold vs warm is visually honest.
	barMax := coldFirst
	for _, s := range warmSamples {
		if s > barMax {
			barMax = s
		}
	}
	if barMax < 10*time.Second {
		barMax = 10 * time.Second
	}

	fmt.Printf("COLD (model loading from disk)\n")
	printResult("cold", coldFirst, barMax)
	fmt.Println()
	fmt.Printf("WARM (model already in VRAM) - %d run(s)\n", *runs)
	for i, ft := range warmSamples {
		printResult(fmt.Sprintf("warm #%d", i+1), ft, barMax)
	}
	fmt.Println()

	// ── Step 4: summary ───────────────────────────────────────────────────────
	if len(warmSamples) == 0 {
		fmt.Println("no warm samples collected")
		os.Exit(1)
	}

	sort.Slice(warmSamples, func(i, j int) bool { return warmSamples[i] < warmSamples[j] })
	var warmMedian time.Duration
	mid := len(warmSamples) / 2
	if len(warmSamples)%2 == 1 {
		warmMedian = warmSamples[mid]
	} else {
		warmMedian = (warmSamples[mid-1] + warmSamples[mid]) / 2
	}
	speedup := float64(coldFirst) / float64(warmMedian)

	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("  cold first-token   : %s\n", fmtDur(coldFirst))
	fmt.Printf("  warm median        : %s\n", fmtDur(warmMedian))
	fmt.Printf("  speedup            : %.1fx faster when warm\n", speedup)
	fmt.Println("─────────────────────────────────────────")
	fmt.Println()
}

// evict sends keep_alive=0 to Ollama so it unloads the model from VRAM.
// Uses a direct HTTP call (not the NDJSON scanner) since we don't need response tokens.
func evict(client *http.Client, endpoint, model, apiKey string) error {
	body := map[string]any{
		"model":      model,
		"prompt":     "",
		"stream":     false,
		"keep_alive": "0s",
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal evict: %w", err)
	}
	req, err := http.NewRequest("POST", endpoint+"/api/generate", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("create evict request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("evict HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// measure sends one streaming generate request and returns time-to-first-token
// and total response time.
func measure(client *http.Client, endpoint, model, prompt, apiKey string) (firstToken, total time.Duration, err error) {
	body := map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": true,
	}
	return doGenerate(client, endpoint, body, apiKey)
}

func doGenerate(client *http.Client, endpoint string, body map[string]any, apiKey string) (firstToken, total time.Duration, err error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequest("POST", endpoint+"/api/generate", bytes.NewReader(b))
	if err != nil {
		return 0, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	var firstRecorded bool
	var lastParseErr error
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var chunk struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			line := scanner.Text()
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			lastParseErr = fmt.Errorf("%w (line: %s)", err, line)
			continue
		}
		if !firstRecorded && chunk.Response != "" {
			firstToken = time.Since(start)
			firstRecorded = true
		}
		if chunk.Done {
			break
		}
	}
	total = time.Since(start)
	if err := scanner.Err(); err != nil {
		return firstToken, total, err
	}
	if !firstRecorded {
		if lastParseErr != nil {
			return 0, total, fmt.Errorf("no tokens received (model may have failed to load): last parse error: %v", lastParseErr)
		}
		return 0, total, fmt.Errorf("no tokens received (model may have failed to load)")
	}
	return firstToken, total, nil
}

func printResult(label string, d, maxDur time.Duration) {
	bar := progressBar(d, maxDur, 40)
	fmt.Printf("  %-10s %s  %s\n", label, fmtDur(d), bar)
}

func fmtDur(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%5.2fs", d.Seconds())
	}
	return fmt.Sprintf("%5dms", d.Milliseconds())
}

// progressBar renders a visual bar scaled to maxDur.
func progressBar(d, maxDur time.Duration, width int) string {
	if d <= 0 || maxDur <= 0 {
		return ""
	}
	filled := int(float64(d) / float64(maxDur) * float64(width))
	if filled > width {
		filled = width
	}
	bar := make([]rune, width)
	for i := range bar {
		if i < filled {
			bar[i] = '█'
		} else {
			bar[i] = '░'
		}
	}
	return "[" + string(bar) + "]"
}
