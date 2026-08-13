// bench/loadtest/main.go - SQLite write-path throughput sweep for ollama-mesh.
//
// Fires a sustained request rate at the mesh proxy for a fixed duration per
// step, sweeping the rate upward across steps, and reports:
//   - target vs. actual-sent vs. completed vs. failed RPS per step (so a
//     generator that can't keep up with its own target is caught, not
//     mistaken for a saturated mesh)
//   - p50/p95/p99 request latency per step
//   - *.db-wal and .db file size sampled at the start/end of each step
//     (passive observation only - this tool never issues PRAGMA
//     wal_checkpoint, which would actively force a checkpoint and perturb
//     the exact WAL-growth behavior under test)
//
// It does NOT compute or print a single "ceiling" number. Read the queue-full
// drop log lines from the mesh's own stdout/log file during the run
// ("audit logger: queue full, ...", "async logger: queue full, ...",
// "async logger: stats queue full, ...") - the first step at which any of
// those appear is the actual operational limit. If none appear across the
// whole swept range, that's a real result too: report "no drop ceiling
// observed up to N req/s", not an invented threshold.
//
// Build (from repo root, requires Go toolchain or Docker):
//
//	go build -o bench/loadtest ./bench/loadtest
//
// This lives in its own subpackage (bench/loadtest/) rather than alongside
// ttft.go, since Go doesn't allow two package-main files with their own
// main() in the same directory/build target.
//
// Usage:
//
//	./bench/loadtest --url http://localhost:11434 --model llama3.2:3b \
//	  --api-key <key> --db mesh.db --rates 5,10,20,40,80 --step-duration 30s
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:11434", "Base URL of the mesh proxy")
	model := flag.String("model", "llama3.2:3b", "Model name to request (must be warm on the target node - see README)")
	apiKey := flag.String("api-key", "", "Bearer API key for the mesh")
	dbPath := flag.String("db", "mesh.db", "Path to the mesh's SQLite database, for passive .db/-wal file size sampling")
	ratesFlag := flag.String("rates", "5,10,20,40,80", "Comma-separated list of target request rates (req/s) to sweep, ascending")
	stepDuration := flag.Duration("step-duration", 20*time.Second, "How long to sustain each rate before moving to the next")
	endpoint := flag.String("endpoint", "chat", "API endpoint: generate or chat")
	generatorSlackPct := flag.Float64("generator-slack-pct", 10, "Max allowed shortfall (%) between target and actual-sent RPS before a step is flagged generator-saturated")
	flag.Parse()

	rates, err := parseRates(*ratesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("SQLite write-path load sweep: url=%s  model=%s  endpoint=%s  step=%s  rates=%v\n\n",
		*url, *model, *endpoint, *stepDuration, rates)
	fmt.Println("This tool reports the latency curve and file-size deltas only. It does NOT")
	fmt.Println("compute a ceiling for you - watch the mesh's own log output during the run for")
	fmt.Println("\"queue full\" / \"dropped\" lines (audit logger, async logger, stats queue).")
	fmt.Println("The first step where one of those appears is the real operational limit. If")
	fmt.Println("none appear across the whole sweep, report \"no drop ceiling observed up to N")
	fmt.Println("req/s\" - never round the highest tested rate up to a claimed ceiling.")
	fmt.Println()

	// http.DefaultTransport caps at MaxIdleConnsPerHost=2, which serializes
	// requests through a tiny connection pool well before the mesh itself is
	// under any real pressure - a client-side bottleneck that would silently
	// masquerade as mesh latency. Raise it well above any tested rate.
	transport := &http.Transport{
		MaxIdleConns:        2000,
		MaxIdleConnsPerHost: 2000,
		MaxConnsPerHost:     0, // unlimited
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: transport}

	fmt.Printf("%-8s  %-10s  %-10s  %-10s  %-10s  %-8s  %-8s  %-8s  %-14s  %-14s\n",
		"rate", "target/s", "sent/s", "done/s", "fail/s", "p50ms", "p95ms", "p99ms", "wal_delta", "db_delta")
	fmt.Println(strings.Repeat("-", 118))

	for _, rate := range rates {
		walBefore, dbBefore := fileSizes(*dbPath)
		result := runStep(client, *url, *model, *endpoint, *apiKey, rate, *stepDuration)
		walAfter, dbAfter := fileSizes(*dbPath)

		warnFlag := ""
		shortfallPct := 0.0
		if rate > 0 {
			shortfallPct = (float64(rate) - result.sentPerSec) / float64(rate) * 100
		}
		if shortfallPct > *generatorSlackPct {
			warnFlag = fmt.Sprintf("  [GENERATOR-SATURATED: sent %.1f%% below target, this step's numbers do not reflect mesh capacity]", shortfallPct)
		}

		fmt.Printf("%-8d  %-10d  %-10.1f  %-10.1f  %-10.1f  %-8.1f  %-8.1f  %-8.1f  %-14s  %-14s%s\n",
			rate, rate, result.sentPerSec, result.donePerSec, result.failPerSec,
			result.p50, result.p95, result.p99,
			deltaStr(walBefore, walAfter), deltaStr(dbBefore, dbAfter), warnFlag)
	}

	fmt.Println()
	fmt.Println("Sweep complete. Cross-reference the mesh's log output for drop lines to find the")
	fmt.Println("actual first-drop rate. Do not publish a number from this table alone as a")
	fmt.Println("\"ceiling\" - it is a latency/file-growth curve, not a threshold verdict.")
}

func parseRates(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	rates := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("invalid rate %q: must be a positive integer", p)
		}
		rates = append(rates, v)
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("--rates produced no values")
	}
	return rates, nil
}

// fileSizes returns (walSize, dbSize) in bytes, or -1 for either that
// doesn't exist yet (e.g. WAL checkpointed empty between steps).
func fileSizes(dbPath string) (int64, int64) {
	wal := statSize(dbPath + "-wal")
	db := statSize(dbPath)
	return wal, db
}

func statSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

func deltaStr(before, after int64) string {
	if before < 0 || after < 0 {
		return "n/a"
	}
	delta := after - before
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%dB", sign, delta)
}

type stepResult struct {
	sentPerSec    float64
	donePerSec    float64
	failPerSec    float64
	p50, p95, p99 float64
}

// runStep sustains target req/s for duration using a fixed worker pool that
// self-throttles to the target rate, recording per-request latency and
// send/complete/fail counts so a generator that can't keep up is visible in
// the output rather than silently masquerading as mesh saturation.
func runStep(client *http.Client, baseURL, model, ep, apiKey string, targetRate int, duration time.Duration) stepResult {
	var sent, done, failed int64
	var latMu sync.Mutex
	var latencies []float64

	interval := time.Second / time.Duration(targetRate)
	stop := time.Now().Add(duration)
	var wg sync.WaitGroup

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for time.Now().Before(stop) {
		<-ticker.C
		atomic.AddInt64(&sent, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			ok := fireRequest(client, baseURL, model, ep, apiKey)
			ms := float64(time.Since(start).Milliseconds())
			if ok {
				atomic.AddInt64(&done, 1)
				latMu.Lock()
				latencies = append(latencies, ms)
				latMu.Unlock()
			} else {
				atomic.AddInt64(&failed, 1)
			}
		}()
	}
	wg.Wait()

	secs := duration.Seconds()
	result := stepResult{
		sentPerSec: float64(atomic.LoadInt64(&sent)) / secs,
		donePerSec: float64(atomic.LoadInt64(&done)) / secs,
		failPerSec: float64(atomic.LoadInt64(&failed)) / secs,
	}

	latMu.Lock()
	sort.Float64s(latencies)
	latMu.Unlock()
	if len(latencies) > 0 {
		result.p50 = percentile(latencies, 50)
		result.p95 = percentile(latencies, 95)
		result.p99 = percentile(latencies, 99)
	}
	return result
}

func fireRequest(client *http.Client, baseURL, model, ep, apiKey string) bool {
	var path string
	var body []byte
	var err error

	switch ep {
	case "generate":
		path = "/api/generate"
		body, err = json.Marshal(map[string]interface{}{
			"model":  model,
			"prompt": "Say one word.",
			"stream": true,
		})
	default:
		path = "/v1/chat/completions"
		body, err = json.Marshal(map[string]interface{}{
			"model":  model,
			"stream": true,
			"messages": []map[string]string{
				{"role": "user", "content": "Say one word."},
			},
		})
	}
	if err != nil {
		return false
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

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
