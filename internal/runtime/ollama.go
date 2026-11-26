package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
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

	// Ollama sends size_vram (snake_case) and a manifest digest per loaded model.
	// size and context_length are kept raw and read by positiveInt, so a missing,
	// null or oddly typed value can never fail the whole probe.
	var ps struct {
		Models []struct {
			Name          string          `json:"name"`
			SizeVRAM      int64           `json:"size_vram"`
			Digest        string          `json:"digest"`
			Size          json.RawMessage `json:"size"`
			ContextLength json.RawMessage `json:"context_length"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return ProbeResult{}, fmt.Errorf("ollama probe decode: %w", err)
	}

	models := make([]LoadedModel, len(ps.Models))
	var totalBytes int64
	for i, m := range ps.Models {
		models[i] = LoadedModel{
			Name: m.Name, SizeVRAMBytes: m.SizeVRAM, Digest: m.Digest,
			SizeBytes: positiveInt(m.Size), ContextLength: positiveInt(m.ContextLength),
		}
		totalBytes += m.SizeVRAM
	}

	return ProbeResult{
		LoadedModels: models,
		VRAMUsedMB:   totalBytes / (1024 * 1024),
	}, nil
}

// positiveInt reads a JSON number as a whole, positive int64. Anything else
// (absent, null, string, bool, object, NaN, infinite, negative, fractional, zero,
// out of range) is unknown and returns 0. It never fails.
func positiveInt(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f != math.Trunc(f) || f >= math.MaxInt64 {
		return 0
	}
	return int64(f)
}
