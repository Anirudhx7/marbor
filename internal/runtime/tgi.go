package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// TGIProbe probes a HuggingFace Text Generation Inference backend.
// Health check: GET /health must return 200.
// Model:        GET /info → model_id field.
// VRAM:         not exposed; always 0.
type TGIProbe struct {
	client *http.Client
}

// Probe checks TGI health then fetches the loaded model name from /info.
func (p *TGIProbe) Probe(ctx context.Context, nodeURL string) (ProbeResult, error) {
	// Step 1: health check.
	if err := checkHealth(ctx, p.client, nodeURL); err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe: %w", err)
	}

	// Step 2: fetch model info.
	req, err := http.NewRequestWithContext(ctx, "GET", nodeURL+"/info", nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe build info request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe info request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("tgi probe: /info returned %d", resp.StatusCode)
	}

	var info struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe decode info: %w", err)
	}

	var models []LoadedModel
	if info.ModelID != "" {
		models = []LoadedModel{{Name: info.ModelID, SizeVRAMBytes: 0}}
	}

	return ProbeResult{LoadedModels: models, VRAMUsedMB: 0}, nil
}
