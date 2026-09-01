package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// VLLMProbe probes a vLLM backend.
// Health check: GET /health must return 200.
// Model list:   GET /v1/models → data[0].id
// VRAM:         not exposed by vLLM API; always 0.
type VLLMProbe struct {
	client *http.Client
}

// Probe checks vLLM health then fetches the loaded model name.
func (p *VLLMProbe) Probe(ctx context.Context, nodeURL string) (ProbeResult, error) {
	base := strings.TrimRight(nodeURL, "/")
	// Step 1: health check.
	if err := checkHealth(ctx, p.client, base); err != nil {
		return ProbeResult{}, fmt.Errorf("vllm probe: %w", err)
	}

	// Step 2: list models.
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/models", nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("vllm probe build models request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("vllm probe models request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("vllm probe: /v1/models returned %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
			// Root is vLLM's OpenAI-compatible ModelCard field carrying the
			// local model path or HF repo id the server was actually started
			// with (see vllm.entrypoints.openai.protocol.ModelCard) - a real
			// field already returned by vLLM's own API, not something this
			// probe invents. It is genuinely useful as a content-identity
			// signal distinct from ID: an operator can serve two different
			// quantized builds (e.g. a Q4 GPTQ checkpoint and an F16
			// checkpoint) under the identical --served-model-name, in which
			// case ID is indistinguishable across nodes but Root (the actual
			// path/repo loaded) still differs. Empty when vLLM's version
			// doesn't populate it - never fabricated (R1).
			Root string `json:"root"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ProbeResult{}, fmt.Errorf("vllm probe decode models: %w", err)
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
