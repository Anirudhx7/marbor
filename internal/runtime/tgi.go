package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	base := strings.TrimRight(nodeURL, "/")
	// Step 1: health check.
	if err := checkHealth(ctx, p.client, base); err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe: %w", err)
	}

	// Step 2: fetch model info.
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/info", nil)
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
		// ModelSha is TGI's own reported HF revision/commit hash for the
		// loaded model (real field on TGI's /info response, present since
		// early releases) - a genuine content-identity signal distinct from
		// ModelID: an operator can serve two different revisions/quantized
		// builds of the same repo under the identical model_id, in which
		// case ModelSha still differs. Empty when TGI's build doesn't
		// report one - never fabricated (R1).
		ModelSha string `json:"model_sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ProbeResult{}, fmt.Errorf("tgi probe decode info: %w", err)
	}

	var models []LoadedModel
	if info.ModelID != "" {
		models = []LoadedModel{{Name: info.ModelID, SizeVRAMBytes: 0, Digest: info.ModelSha}}
	}

	return ProbeResult{LoadedModels: models, VRAMUsedMB: 0}, nil
}
