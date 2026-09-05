// Package bench implements the "marbor bench" subcommand.
//
// It measures cold vs warm Time-To-First-Token (TTFT) through the marbor proxy
// using the OpenAI-compatible /v1/chat/completions streaming endpoint.
// The --target flag must point at the marbor proxy port (e.g.
// http://localhost:11434), NOT at an Ollama backend directly - we measure
// what the marbor adds, not raw Ollama speed.
//
// Usage:
//
//	marbor bench --target http://localhost:11434 [--model llama3:8b] [--json] [--key <api-key>] [--timeout 120s]
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Result holds the measured cold and warm TTFT values, plus TPOT
// (time-per-output-token) when it was computable. ColdTPOTMs/
// WarmTPOTMs are nil when the corresponding sample carried fewer than 2
// content-bearing SSE chunks - TPOT genuinely can't be computed from a
// single-token response, so this is absence, not a fabricated 0.
type Result struct {
	Model          string   `json:"model"`
	ColdMs         int64    `json:"cold_ms"`
	WarmMs         int64    `json:"warm_ms"`
	ColdTPOTMs     *float64 `json:"cold_tpot_ms,omitempty"`
	WarmTPOTMs     *float64 `json:"warm_tpot_ms,omitempty"`
	ImprovementX   float64  `json:"improvement_x"`
	ImprovementPct float64  `json:"improvement_pct"`
}

// Run is the entry-point for the "bench" subcommand.  args is os.Args[2:].
func Run(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "Usage: marbor bench [flags]\n\n")
		fmt.Fprintf(w, "Measures cold vs warm Time-To-First-Token (TTFT) through the marbor proxy.\n")
		fmt.Fprintf(w, "--target must point at the marbor proxy port, not an Ollama backend directly.\n\n")
		fmt.Fprintf(w, "Flags:\n")
		fs.SetOutput(w)
		fs.PrintDefaults()
	}
	fs.Usage = func() { usage(os.Stderr) }

	target := fs.String("target", "http://localhost:11434", "Marbor proxy base URL (not the Ollama backend)")
	model := fs.String("model", "", "Model to benchmark (auto-detected from /v1/models if omitted)")
	apiKey := fs.String("key", "", "Bearer API key (required if auth is enabled on the marbor)")
	jsonOut := fs.Bool("json", false, "Emit JSON output instead of the human-readable table")
	timeout := fs.Duration("timeout", 300*time.Second, "Per-request timeout (cold load can take minutes on a large model)")

	// -h/--help must be intercepted before fs.Parse runs: flag's own usage
	// hook fires identically for a genuine bad-flag error and for a help
	// request, so routing help to stdout (vs. stderr for real errors) has to
	// be decided before Parse, not after (same pattern as internal/cli's
	// parseFlags).
	for _, a := range args {
		if a == "-h" || a == "--help" {
			usage(os.Stdout)
			return
		}
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	*target = strings.TrimRight(*target, "/")

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
	coldSample, err := MeasureChatLatency(context.Background(), client, *target, resolvedModel, *apiKey)
	_ = coldStart
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: cold request failed: %v\n", err)
		os.Exit(1)
	}

	// ── 3. Warm TTFT (immediate repeat - model now in VRAM) ─────────────────
	if !*jsonOut {
		fmt.Printf("Sending warm request (model in VRAM)...\n\n")
	}
	warmSample, err := MeasureChatLatency(context.Background(), client, *target, resolvedModel, *apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: warm request failed: %v\n", err)
		os.Exit(1)
	}
	coldMs, warmMs := coldSample.TTFTMs, warmSample.TTFTMs

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
		ColdTPOTMs:     coldSample.TPOTMs,
		WarmTPOTMs:     warmSample.TPOTMs,
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
	var improvStr string
	switch {
	case r.WarmMs <= 0:
		improvStr = "n/a (warm TTFT measured as 0ms)"
	case r.ImprovementPct < 0:
		improvStr = "n/a (warm was slower than cold - noisy sample?)"
	default:
		improvStr = fmt.Sprintf("%.0fx faster (%.1f%%)", r.ImprovementX, r.ImprovementPct)
	}

	// Column widths for alignment.
	labelW := 13
	fmt.Printf("\n")
	fmt.Printf("  %-*s %s\n", labelW, "Model:", r.Model)
	fmt.Printf("  %-*s %s\n", labelW, "Cold TTFT:", fmtMs(r.ColdMs))
	fmt.Printf("  %-*s %s\n", labelW, "Warm TTFT:", fmtMs(r.WarmMs))
	fmt.Printf("  %-*s %s\n", labelW, "Cold TPOT:", fmtTPOT(r.ColdTPOTMs))
	fmt.Printf("  %-*s %s\n", labelW, "Warm TPOT:", fmtTPOT(r.WarmTPOTMs))
	fmt.Printf("  %-*s %s\n", labelW, "Improvement:", improvStr)
	fmt.Printf("\n")
}

// fmtTPOT formats a nullable TPOT sample for the human-readable table. "-"
// when nil (not computable from that sample's stream - absence, never a
// fabricated value), matching this project's honest-data convention.
func fmtTPOT(ms *float64) string {
	if ms == nil {
		return "- (not computable: response had fewer than 2 output tokens)"
	}
	return fmt.Sprintf("%.1fms/token", *ms)
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
	return math.Round(v*10) / 10
}

// detectModel calls GET /v1/models on marbor and returns the first model ID.
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
		return "", fmt.Errorf("no models available on the marbor (is any Ollama node healthy?)")
	}
	return list.Data[0].ID, nil
}

// LatencySample captures real per-request timing from one streaming chat
// completion: TTFT (time-to-first-token, existing measurement) plus TPOT
// (time-per-output-token) derived from real decode-phase chunk timestamps.
// TPOTMs is nil when the stream carried fewer than 2 content-bearing chunks -
// TPOT is not computable for a single-token (or empty) response, and this is
// absence, never a fabricated value.
type LatencySample struct {
	TTFTMs int64
	TPOTMs *float64
}

// MeasureChatTTFT sends a single streaming /v1/chat/completions request
// through marbor and returns the milliseconds until the first non-empty
// token arrives in the SSE stream. Exported so internal/admin's in-dashboard
// hardware benchmark page can reuse the exact same measurement logic instead
// of duplicating it.
//
// This is a thin wrapper around MeasureChatLatency for callers that only
// need TTFT; its own behavior (return as soon as TTFT is captured, without
// draining the rest of the stream) is unchanged from before MeasureChatLatency
// existed - see MeasureChatLatency's own doc comment for why draining the
// full stream to also compute TPOT is a separate, opt-in code path.
func MeasureChatTTFT(ctx context.Context, client *http.Client, target, model, apiKey string) (int64, error) {
	ttftOnly, err := measureChatSSE(ctx, client, target, model, apiKey, false)
	if err != nil {
		return 0, err
	}
	return ttftOnly.TTFTMs, nil
}

// MeasureChatLatency sends a single streaming /v1/chat/completions request
// through marbor and returns both TTFT and TPOT for that one request.
//
// Using the OpenAI-compatible endpoint ensures the request travels through
// the full marbor routing stack (proxy → router → backend), not a direct hop
// to an Ollama node.
//
// Unlike MeasureChatTTFT, this reads the SSE stream through to [DONE]/EOF so
// TPOT can be derived: TPOTMs = (timestamp of last content chunk - timestamp
// of first content chunk) / (count of content chunks - 1), all from real
// wall-clock timestamps on real observed chunks - consistent with the
// industry ITL/TPOT definition. If fewer than 2 content-bearing chunks
// arrive, TPOTMs is nil (absence, never a guess).
//
// ctx bounds the whole call, not just a client.Timeout: internal/admin's
// benchmark job cancels this context immediately on admin cancel, so a
// slow/stuck cold load (which can legitimately take minutes) aborts right
// away instead of only after client.Do eventually returns on its own -
// without this, a cancelled job's deferred ephemeral-key cleanup would wait
// out the in-flight request first, leaving a live key on the wire for the
// remainder of that window.
func MeasureChatLatency(ctx context.Context, client *http.Client, target, model, apiKey string) (LatencySample, error) {
	return measureChatSSE(ctx, client, target, model, apiKey, true)
}

// measureChatSSE is the shared implementation behind MeasureChatTTFT and
// MeasureChatLatency. When drainForTPOT is false it returns as soon as TTFT
// is captured (the original MeasureChatTTFT behavior, unchanged) without
// reading the rest of the stream - the deferred resp.Body.Close() below
// closes it without draining, so the connection isn't reused for a
// subsequent call anyway (each call dials fresh rather than racing a
// background drain against Close). When drainForTPOT is true it keeps
// reading every content-bearing chunk (timestamping each) through
// [DONE]/EOF so TPOT can be computed from real inter-chunk timing.
func measureChatSSE(ctx context.Context, client *http.Client, target, model, apiKey string, drainForTPOT bool) (LatencySample, error) {
	payload := map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "Say one word."},
		},
	}
	if drainForTPOT {
		// Only the TPOT-computing path reads the stream to completion, so
		// only it needs a bound on how long that can take: nothing in this
		// payload otherwise limits response length, and a model/runtime that
		// ignores "Say one word." would make every one of a run's up to 100
		// samples wait for a full, unbounded generation instead of the
		// handful of tokens this measurement actually needs. Runtimes that
		// don't recognize max_tokens simply ignore the field (still bounded
		// in practice by the prompt itself on those).
		payload["max_tokens"] = 16
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return LatencySample{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return LatencySample{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return LatencySample{}, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return LatencySample{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Parse SSE stream: lines are "data: <json>" or "data: [DONE]".
	// Record TTFT on the first chunk that carries a non-empty token, then
	// (only if drainForTPOT) keep recording a real timestamp on every
	// subsequent content-bearing chunk through [DONE]/EOF.
	var ttftMs int64
	var ttftCaptured bool
	var contentTimestamps []time.Time
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
			now := time.Now()
			if !ttftCaptured {
				ttftMs = now.Sub(start).Milliseconds()
				ttftCaptured = true
				if !drainForTPOT {
					// Return immediately once TTFT is captured - same
					// behavior MeasureChatTTFT always had.
					return LatencySample{TTFTMs: ttftMs}, nil
				}
			}
			contentTimestamps = append(contentTimestamps, now)
		}
	}
	if err := scanner.Err(); err != nil {
		return LatencySample{}, fmt.Errorf("read stream: %w", err)
	}
	if !ttftCaptured {
		return LatencySample{}, fmt.Errorf("no tokens received - model may have failed to load, or marbor has no healthy nodes")
	}

	sample := LatencySample{TTFTMs: ttftMs}
	if n := len(contentTimestamps); n >= 2 {
		// Divide the full-precision duration (nanoseconds), not a
		// millisecond-truncated one - truncating first can round a real
		// sub-millisecond elapsed span down to 0 before the division ever
		// runs, reporting an exact 0ms/token that looks like a real
		// measurement instead of the imprecision it actually is.
		totalMs := contentTimestamps[n-1].Sub(contentTimestamps[0]).Seconds() * 1000
		tpot := totalMs / float64(n-1)
		sample.TPOTMs = &tpot
	}
	return sample, nil
}
