// Package runtime provides per-backend health and warm-model probing.
// The Router imports this package; this package MUST NOT import internal/router.
package runtime

import (
	"context"
	"net/http"
)

// LoadedModel describes a single model currently loaded in a backend's VRAM.
type LoadedModel struct {
	Name          string
	SizeVRAMBytes int64 // 0 when unknown
	// Digest is the backend-reported content digest/checksum, when it exposes one
	// (currently only Ollama's /api/ps). Empty when unknown - never fabricated.
	Digest string
}

// ProbeResult is returned by every RuntimeProbe implementation.
type ProbeResult struct {
	LoadedModels []LoadedModel
	VRAMUsedMB   int64 // sum(SizeVRAMBytes) / (1024*1024); 0 is valid when unknown
}

// RuntimeProbe abstracts per-backend health + warm-model detection.
type RuntimeProbe interface {
	Probe(ctx context.Context, nodeURL string) (ProbeResult, error)
}

// NewProbe returns the correct RuntimeProbe for the given runtime string.
// Known values: "ollama" (or ""), "vllm", "tgi", "llamacpp", "mlx".
// Any unknown value falls back to OllamaProbe so existing deployments are safe.
func NewProbe(runtime string, client *http.Client) RuntimeProbe {
	switch runtime {
	case "vllm":
		return &VLLMProbe{client: client}
	case "tgi":
		return &TGIProbe{client: client}
	case "llamacpp":
		return &LlamaCppProbe{client: client}
	case "mlx":
		return &MLXProbe{client: client}
	default:
		// "ollama", "", or any unrecognised value → Ollama (safe default)
		return &OllamaProbe{client: client}
	}
}
