// Package runtime provides per-backend health and warm-model probing.
// The Router imports this package; this package MUST NOT import internal/router.
package runtime

import (
	"context"
	"net/http"
)

// LoadedModel describes a single model currently loaded in a backend's VRAM.
type LoadedModel struct {
	Name string
	// SizeVRAMBytes is the bytes held in GPU memory. 0 is a real reading (nothing on
	// the GPU) when the backend also reports a model size; without a size it means unknown.
	SizeVRAMBytes int64
	// Digest is the backend-reported content digest/checksum, when it exposes one
	// (currently only Ollama's /api/ps). Empty when unknown - never fabricated.
	Digest string
	// SizeBytes is the model's total loaded size and ContextLength the context window it
	// is loaded at, as Ollama reports them. 0 means the backend did not report a
	// usable value (every other runtime, and older Ollama builds) - never fabricated.
	SizeBytes     int64
	ContextLength int64
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
