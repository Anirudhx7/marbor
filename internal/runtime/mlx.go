package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// MLXProbe probes an mlx_lm.server backend (Apple Silicon, ml-explore/mlx-lm).
// Health check: mlx_lm.server exposes no dedicated /health route (unlike
// vLLM/TGI/llama.cpp), so a successful GET /v1/models response IS the
// reachability signal here - never assumed, never a second unverified
// endpoint guess.
// Model list:   GET /v1/models -> data[0].id (same OpenAI-compatible shape
// as vLLM/llama.cpp).
// VRAM:         not exposed by mlx_lm.server's API; always 0.
type MLXProbe struct {
	client *http.Client
}

// Probe fetches the loaded model name from mlx_lm.server's /v1/models.
func (p *MLXProbe) Probe(ctx context.Context, nodeURL string) (ProbeResult, error) {
	base := strings.TrimRight(nodeURL, "/")

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/models", nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("mlx probe build models request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("mlx probe: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("mlx probe: /v1/models returned %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProbeResult{}, fmt.Errorf("mlx probe decode models: %w", err)
	}

	models := make([]LoadedModel, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			models = append(models, LoadedModel{Name: d.ID, SizeVRAMBytes: 0})
		}
	}

	return ProbeResult{LoadedModels: models, VRAMUsedMB: 0}, nil
}
