package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DetectRuntime probes nodeURL and returns the detected runtime string plus
// whether the node was actually contacted. reached=false means every probe
// failed at the transport level (network blip, node still booting) - the
// "ollama" fallback in that case is provisional, not a real identification,
// and callers should not commit it permanently. reached=true with runtime
// "ollama" means the node responded but matched no known runtime signature
// (the genuine "unidentifiable -> ollama" case). Kept as the two-value
// signature the router's own auto-detect caller (health.go) already
// consumes - DetectRuntimeConfirmed below adds the third state DetectAll
// needs (P149) on top of P146's already-landed reached=false fix for the
// ambiguous empty-/v1/models-data case (which does change what this
// function returns for that one input, intentionally - see probeV1Models).
// P149 itself adds no further behavior change beyond that: confirmed is a
// new derived value, never fed back into runtime/reached above.
func DetectRuntime(ctx context.Context, nodeURL string, client *http.Client) (runtime string, reached bool) {
	runtime, reached, _ = DetectRuntimeConfirmed(ctx, nodeURL, client)
	return runtime, reached
}

// DetectRuntimeConfirmed is DetectRuntime's underlying implementation, adding
// a third state (P149): confirmed=true means runtime was identified by an
// actual signature match (ollama's /api/ps, tgi's /info model_id, vllm's
// owned_by field, or llama.cpp's non-empty /v1/models data); confirmed=false
// with runtime=="ollama" means the node responded (reached=true) but matched
// no known signature - a guess, not a real identification. DetectAll uses
// this to skip appending a DetectedRuntime for the unidentified case, so a
// non-runtime HTTP service on a candidate port never gets permanently
// labeled and ID-registered as "ollama." DetectRuntime's own two-value
// signature deliberately does not distinguish this: the router's auto-detect
// caller already treats reached=true+"ollama" as a valid (if generic)
// commit, and changing that behavior was out of scope for this fix.
func DetectRuntimeConfirmed(ctx context.Context, nodeURL string, client *http.Client) (runtime string, reached bool, confirmed bool) {
	base := strings.TrimRight(nodeURL, "/")

	// Ollama: unique /api/ps endpoint
	matched, ok := probeEndpoint(ctx, base+"/api/ps", client)
	reached = reached || ok
	if matched {
		return "ollama", true, true
	}

	// TGI: unique /info endpoint with model_id field
	matched, ok = probeTGIInfo(ctx, base+"/info", client)
	reached = reached || ok
	if matched {
		return "tgi", true, true
	}

	// vLLM vs llama.cpp: both have /v1/models. MLX's /v1/models response is
	// byte-for-byte identical to llama.cpp's here (same shape, no owned_by or
	// other distinguishing field) - there is no signature to probe for, so an
	// MLX node is unavoidably detected as "llamacpp" and then stays
	// permanently unhealthy (llama.cpp's health probe needs a /health
	// endpoint MLX doesn't expose). MLX must be set manually via an explicit
	// runtime (never "auto") - router.go's PatchNode/AddNode set
	// autoDetect=false whenever the runtime is set explicitly, and
	// health.go's needsDetect only re-probes when autoDetect is true AND
	// Runtime=="auto", so a manually-tagged mlx runtime is never silently
	// overwritten by a later auto-detect re-run guessing llamacpp.
	// vLLM sets owned_by to "vllm"; llama.cpp omits it or sets different value
	detected, ok := probeV1Models(ctx, base+"/v1/models", client)
	reached = reached || ok
	if detected != "" {
		return detected, true, true
	}

	return "ollama", reached, false
}

// probeEndpoint reports (matched, reached): matched is true on HTTP 200;
// reached is true whenever the request actually got an HTTP response, even a
// non-200 one, distinguishing "node answered but isn't Ollama" from "node
// was unreachable."
func probeEndpoint(ctx context.Context, url string, client *http.Client) (matched, reached bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, true
}

func probeTGIInfo(ctx context.Context, url string, client *http.Client) (matched, reached bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return false, true
	}
	defer resp.Body.Close()
	var info struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false, true
	}
	return info.ModelID != "", true
}

func probeV1Models(ctx context.Context, url string, client *http.Client) (runtime string, reached bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", true
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", true
	}
	if len(result.Data) > 0 && result.Data[0].OwnedBy == "vllm" {
		return "vllm", true
	}
	if len(result.Data) > 0 {
		return "llamacpp", true
	}
	// /v1/models responded 200 with an empty data array: a genuine vLLM
	// server always lists its model in data[0], so this isn't a real vLLM
	// signature match - it's ambiguous (could be either backend mid-startup,
	// or something else entirely). Return reached=false (P146) so the caller
	// treats this the same as a transport-level failure - leave autoDetect
	// pending and retry next poll - rather than committing a guessed "vllm"
	// permanently.
	return "", false
}
