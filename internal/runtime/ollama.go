package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OllamaProbe probes an Ollama backend via GET /api/ps.
type OllamaProbe struct {
	client *http.Client
}

// Probe calls GET {nodeURL}/api/ps and returns all loaded models with VRAM usage.
func (p *OllamaProbe) Probe(ctx context.Context, nodeURL string) (ProbeResult, error) {
	url := strings.TrimRight(nodeURL, "/") + "/api/ps"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("ollama probe build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("ollama probe request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("ollama probe: /api/ps returned %d", resp.StatusCode)
	}

	// Ollama sends size_vram (snake_case) per loaded model.
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return ProbeResult{}, fmt.Errorf("ollama probe decode: %w", err)
	}

	models := make([]LoadedModel, len(ps.Models))
	var totalBytes int64
	for i, m := range ps.Models {
		models[i] = LoadedModel{Name: m.Name, SizeVRAMBytes: m.SizeVRAM}
		totalBytes += m.SizeVRAM
	}

	return ProbeResult{
		LoadedModels: models,
		VRAMUsedMB:   totalBytes / (1024 * 1024),
	}, nil
}
