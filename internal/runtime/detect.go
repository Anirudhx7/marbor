package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DetectRuntime probes nodeURL and returns the detected runtime string.
// Returns "ollama" if detection fails (safe default).
// Detection order: Ollama (/api/ps) -> TGI (/info) -> vLLM/llama.cpp (/v1/models).
func DetectRuntime(ctx context.Context, nodeURL string, client *http.Client) string {
	base := strings.TrimRight(nodeURL, "/")

	// Ollama: unique /api/ps endpoint
	if probeEndpoint(ctx, base+"/api/ps", client) {
		return "ollama"
	}

	// TGI: unique /info endpoint with model_id field
	if probeTGIInfo(ctx, base+"/info", client) {
		return "tgi"
	}

	// vLLM vs llama.cpp: both have /v1/models
	// vLLM sets owned_by to "vllm"; llama.cpp omits it or sets different value
	detected := probeV1Models(ctx, base+"/v1/models", client)
	if detected != "" {
		return detected
	}

	return "ollama"
}

func probeEndpoint(ctx context.Context, url string, client *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func probeTGIInfo(ctx context.Context, url string, client *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	var info struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return false
	}
	return info.ModelID != ""
}

func probeV1Models(ctx context.Context, url string, client *http.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	if len(result.Data) > 0 && result.Data[0].OwnedBy == "vllm" {
		return "vllm"
	}
	if len(result.Data) > 0 {
		return "llamacpp"
	}
	// /v1/models responded but empty data - could be either; call it vllm
	return "vllm"
}
