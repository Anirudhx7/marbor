package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// LlamaCppProbe probes a llama.cpp server backend.
// Health check: GET /health must return 200.
// Model list:   GET /v1/models → data[0].id (same OpenAI-compatible shape as vLLM).
// VRAM:         not exposed; always 0.
type LlamaCppProbe struct {
	client *http.Client
}

// Probe checks llama.cpp health then fetches the loaded model name.
func (p *LlamaCppProbe) Probe(ctx context.Context, nodeURL string) (ProbeResult, error) {
	base := strings.TrimRight(nodeURL, "/")
	// Step 1: health check.
	if err := checkHealth(ctx, p.client, base); err != nil {
		return ProbeResult{}, fmt.Errorf("llamacpp probe: %w", err)
	}

	// Step 2: list models (OpenAI-compatible /v1/models).
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/models", nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("llamacpp probe build models request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("llamacpp probe models request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("llamacpp probe: /v1/models returned %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
			// Root mirrors the same optional field vLLM's OpenAI-compatible
			// /v1/models returns (the local model path/repo actually
			// loaded). Not confirmed present across all llama.cpp server
			// versions (its /v1/models implementation is hand-rolled, not
			// vLLM's protocol) - parsed defensively so it's used when a
			// build happens to report it and silently ignored (Digest stays
			// empty, today's behavior) when absent. Never fabricated.
			Root string `json:"root"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProbeResult{}, fmt.Errorf("llamacpp probe decode models: %w", err)
	}

	models := make([]LoadedModel, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			digest := ""
			if d.Root != "" && d.Root != d.ID {
				digest = d.Root
			}
			models = append(models, LoadedModel{Name: d.ID, SizeVRAMBytes: 0, Digest: digest})
		}
	}

	return ProbeResult{LoadedModels: models, VRAMUsedMB: 0}, nil
}
