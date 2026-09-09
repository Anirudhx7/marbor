// Package marboragent: enginestate.go scrapes a co-located inference
// runtime's own native /metrics endpoint for the three EngineState metrics
// (RunningRequests, WaitingRequests, KVCacheUsagePercent) - a materially
// different source than the generic OpenAI-compatible probe in
// internal/runtime (which answers "is it up, what's loaded", never engine
// scheduler state). Localhost-only, piggybacking the same refresh tick
// scheduler.go already runs - no new polling loop, no new dependency (this
// project ships as a single static binary with zero external Go
// dependencies, so Prometheus text format is hand-parsed here rather than
// importing client_golang's expfmt).
//
// Per-runtime coverage, verified against each runtime's own public metrics
// documentation on 2026-09-09 (not a live endpoint - no vLLM/TGI instance
// was reachable from this build environment; treat metric names as
// documented, not live-confirmed, until exercised against a real server):
//   - vLLM: vllm:num_requests_running / vllm:num_requests_waiting are the
//     same names on both the legacy and V1 engine. The KV-cache metric name
//     differs: legacy exposes vllm:gpu_cache_usage_perc, V1 exposes
//     vllm:kv_cache_usage_perc - both a 0-1 fraction per vLLM's own metrics
//     design doc, converted to a 0-100 percent here. Source:
//     https://docs.vllm.ai/en/latest/design/v1/metrics.html
//   - TGI: tgi_batch_current_size ("Current batch size") maps to
//     RunningRequests and tgi_queue_size ("Current queue size") maps to
//     WaitingRequests - both documented with unambiguous semantics. TGI
//     exposes no KV-cache-utilization equivalent at all; KVCacheUsagePercent
//     stays nil for TGI rather than estimated from an unrelated gauge like
//     tgi_batch_current_max_tokens (do not manufacture cross-runtime
//     symmetry). Source:
//     https://huggingface.co/docs/text-generation-inference/en/reference/metrics
//   - Ollama: no Prometheus endpoint exists - deliberately deferred,
//     EngineState stays nil.
//   - llama.cpp (llama-server): some builds expose a metrics endpoint
//     behind a build/run flag, but which flag and which metric names is
//     unverified - deliberately deferred rather than guessed, EngineState
//     stays nil.
//   - MLX: no metrics surface - deliberately deferred, EngineState stays nil.
package marboragent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// collectEngineState dispatches to the per-runtime scraper for name, or nil
// for any runtime with no verified native metrics source (see the package
// doc comment's coverage table) - never a guessed mapping for an
// unrecognized runtime name.
func collectEngineState(ctx context.Context, client *http.Client, name, baseURL string) *EngineState {
	switch name {
	case "vllm":
		return collectVLLMEngineState(ctx, client, baseURL)
	case "tgi":
		return collectTGIEngineState(ctx, client, baseURL)
	default:
		return nil
	}
}

func collectVLLMEngineState(ctx context.Context, client *http.Client, baseURL string) *EngineState {
	samples, err := fetchPrometheusSamples(ctx, client, baseURL)
	if err != nil {
		return nil
	}
	es := &EngineState{}
	if v, ok := sumSamples(samples, "vllm:num_requests_running"); ok {
		n := int(v)
		es.RunningRequests = &n
	}
	if v, ok := sumSamples(samples, "vllm:num_requests_waiting"); ok {
		n := int(v)
		es.WaitingRequests = &n
	}
	// KV-cache metric name differs between the legacy and V1 engine (see
	// package doc comment) - try V1's name first, fall back to legacy. Both
	// report a 0-1 fraction, converted to a 0-100 percent.
	if v, ok := sumSamples(samples, "vllm:kv_cache_usage_perc"); ok {
		es.KVCacheUsagePercent = clampPercent(v * 100)
	} else if v, ok := sumSamples(samples, "vllm:gpu_cache_usage_perc"); ok {
		es.KVCacheUsagePercent = clampPercent(v * 100)
	}
	if es.RunningRequests == nil && es.WaitingRequests == nil && es.KVCacheUsagePercent == nil {
		return nil
	}
	return es
}

func collectTGIEngineState(ctx context.Context, client *http.Client, baseURL string) *EngineState {
	samples, err := fetchPrometheusSamples(ctx, client, baseURL)
	if err != nil {
		return nil
	}
	es := &EngineState{}
	if v, ok := sumSamples(samples, "tgi_batch_current_size"); ok {
		n := int(v)
		es.RunningRequests = &n
	}
	if v, ok := sumSamples(samples, "tgi_queue_size"); ok {
		n := int(v)
		es.WaitingRequests = &n
	}
	// TGI exposes no KV-cache-utilization equivalent - left nil, never
	// estimated from an unrelated gauge (e.g. tgi_batch_current_max_tokens).
	if es.RunningRequests == nil && es.WaitingRequests == nil {
		return nil
	}
	return es
}

// fetchPrometheusSamples GETs baseURL+"/metrics" and parses it with the
// minimal Prometheus text-format scanner below. Any failure (unreachable,
// non-200, unparseable body) returns an error so the caller reports the
// whole EngineState as nil rather than a partially-fabricated one - an
// engine that answers with garbage is exactly as unknown as one that never
// answered.
func fetchPrometheusSamples(ctx context.Context, client *http.Client, baseURL string) (map[string][]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned status %d", resp.StatusCode)
	}
	// 4MiB cap: a rich vLLM /metrics page runs a few hundred KB - this is a
	// sanity bound against a misbehaving endpoint, not a tuned limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parsePrometheusText(body), nil
}

// parsePrometheusText does the minimal single-pass parse this package needs
// (bare gauge samples, no histogram bucket/quantile handling) instead of
// depending on client_golang's expfmt, keeping this a zero-dependency
// binary. Returns every
// sample keyed by its bare metric name with any "{...}" label suffix
// stripped - a metric that appears more than once (e.g. per-model labels)
// keeps every value so the caller can sum them.
func parsePrometheusText(body []byte) map[string][]float64 {
	out := make(map[string][]float64)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		out[name] = append(out[name], v)
	}
	return out
}

// clampPercent bounds a reported percentage to [0,100] - a runtime's own
// gauge is trusted data, not user input, but a transient reading fractionally
// outside its documented 0-1 range (rounding, a mid-scrape counter reset)
// should not surface as a nonsensical "-4%" or "103%" on the dashboard. This
// clamps display range only; it never substitutes a value the runtime did
// not report (that stays nil, per the honest-unknown rule above).
func clampPercent(v float64) *float64 {
	if v < 0 {
		v = 0
	} else if v > 100 {
		v = 100
	}
	return &v
}

// sumSamples reports whether name was present at all this scrape (ok=false
// means unknown, never conflated with a real value of 0) and, when present,
// the sum of every sample under that name.
func sumSamples(samples map[string][]float64, name string) (float64, bool) {
	vals, ok := samples[name]
	if !ok || len(vals) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum, true
}
