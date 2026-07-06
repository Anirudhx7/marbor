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
// (the genuine "unidentifiable -> ollama" case).
func DetectRuntime(ctx context.Context, nodeURL string, client *http.Client) (runtime string, reached bool) {
	base := strings.TrimRight(nodeURL, "/")

	// Ollama: unique /api/ps endpoint
	matched, ok := probeEndpoint(ctx, base+"/api/ps", client)
	reached = reached || ok
	if matched {
		return "ollama", true
	}

	// TGI: unique /info endpoint with model_id field
	matched, ok = probeTGIInfo(ctx, base+"/info", client)
	reached = reached || ok
	if matched {
		return "tgi", true
	}

	// vLLM vs llama.cpp: both have /v1/models
	// vLLM sets owned_by to "vllm"; llama.cpp omits it or sets different value
	detected, ok := probeV1Models(ctx, base+"/v1/models", client)
	reached = reached || ok
	if detected != "" {
		return detected, true
	}

	return "ollama", reached
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
	// /v1/models responded but empty data - could be either; call it vllm
	return "vllm", true
}
