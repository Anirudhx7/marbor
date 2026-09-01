package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// helper: default http.Client (no custom timeouts needed in tests).
func testClient() *http.Client { return &http.Client{} }

// --- OllamaProbe tests ---

func TestOllamaProbe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3:8b", "size_vram": 8 * 1024 * 1024 * 1024},  // 8 GiB
				{"name": "mistral:7b", "size_vram": 4 * 1024 * 1024 * 1024}, // 4 GiB
			},
		})
	}))
	defer srv.Close()

	probe := &OllamaProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(result.LoadedModels) != 2 {
		t.Fatalf("expected 2 models, got %d", len(result.LoadedModels))
	}
	if result.LoadedModels[0].Name != "llama3:8b" {
		t.Errorf("expected llama3:8b, got %s", result.LoadedModels[0].Name)
	}
	if result.LoadedModels[1].Name != "mistral:7b" {
		t.Errorf("expected mistral:7b, got %s", result.LoadedModels[1].Name)
	}
	// 12 GiB total = 12 * 1024 MB
	expectedMB := int64(12 * 1024)
	if result.VRAMUsedMB != expectedMB {
		t.Errorf("expected VRAMUsedMB=%d, got %d", expectedMB, result.VRAMUsedMB)
	}
}

func TestOllamaProbe_DigestParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3:8b", "size_vram": 8 * 1024 * 1024 * 1024, "digest": "sha256:abc123"},
				{"name": "mistral:7b", "size_vram": 4 * 1024 * 1024 * 1024},
			},
		})
	}))
	defer srv.Close()

	probe := &OllamaProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "sha256:abc123" {
		t.Errorf("expected digest sha256:abc123, got %q", result.LoadedModels[0].Digest)
	}
	if result.LoadedModels[1].Digest != "" {
		t.Errorf("expected empty digest when runtime omits it, got %q", result.LoadedModels[1].Digest)
	}
}

func TestOllamaProbe_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := &OllamaProbe{client: testClient()}
	_, err := probe.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503, got nil")
	}
}

// --- VLLMProbe tests ---

func TestVLLMProbe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "meta-llama/Llama-3-8B-Instruct"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &VLLMProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(result.LoadedModels) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.LoadedModels))
	}
	if result.LoadedModels[0].Name != "meta-llama/Llama-3-8B-Instruct" {
		t.Errorf("unexpected model name: %s", result.LoadedModels[0].Name)
	}
	if result.VRAMUsedMB != 0 {
		t.Errorf("expected VRAMUsedMB=0 for vLLM, got %d", result.VRAMUsedMB)
	}
}

// TestVLLMProbe_RootDigest_DistinguishesSameServedName (P406): two nodes
// serving different underlying weights under the identical served model
// name are only distinguishable once vLLM's own "root" field (the actual
// local path/repo loaded) differs - this test locks in that Digest is
// populated from Root when Root differs from ID, and stays empty when the
// runtime doesn't report Root at all (today's behavior, must not regress).
func TestVLLMProbe_RootDigest_DistinguishesSameServedName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama-3-70b", "root": "/models/llama-3-70b-q4_k_m.gguf"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &VLLMProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "/models/llama-3-70b-q4_k_m.gguf" {
		t.Errorf("expected Digest populated from root, got %q", result.LoadedModels[0].Digest)
	}
}

func TestVLLMProbe_NoRoot_DigestEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama-3-70b"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &VLLMProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "" {
		t.Errorf("expected empty Digest when root is absent, got %q", result.LoadedModels[0].Digest)
	}
}

func TestVLLMProbe_HealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := &VLLMProbe{client: testClient()}
	_, err := probe.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503 /health, got nil")
	}
}

// --- TGIProbe tests ---

func TestTGIProbe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/info":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"model_id": "mistralai/Mistral-7B-Instruct-v0.2",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &TGIProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(result.LoadedModels) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.LoadedModels))
	}
	if result.LoadedModels[0].Name != "mistralai/Mistral-7B-Instruct-v0.2" {
		t.Errorf("unexpected model name: %s", result.LoadedModels[0].Name)
	}
	if result.VRAMUsedMB != 0 {
		t.Errorf("expected VRAMUsedMB=0 for TGI, got %d", result.VRAMUsedMB)
	}
}

// TestTGIProbe_ModelShaDigest (P406): TGI's /info model_sha is a real
// content-identity signal (HF revision hash) distinct from model_id -
// two revisions/quant builds served under the identical model_id must be
// distinguished by it once present.
func TestTGIProbe_ModelShaDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/info":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"model_id":  "mistralai/Mistral-7B-Instruct-v0.2",
				"model_sha": "abc123def456",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &TGIProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "abc123def456" {
		t.Errorf("expected Digest=abc123def456 from model_sha, got %q", result.LoadedModels[0].Digest)
	}
}

func TestTGIProbe_NoModelSha_DigestEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/info":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"model_id": "mistralai/Mistral-7B-Instruct-v0.2",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &TGIProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "" {
		t.Errorf("expected empty Digest when model_sha is absent, got %q", result.LoadedModels[0].Digest)
	}
}

func TestTGIProbe_HealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := &TGIProbe{client: testClient()}
	_, err := probe.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503 /health, got nil")
	}
}

// --- LlamaCppProbe tests ---

func TestLlamaCppProbe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama-3.1-8b-instruct"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &LlamaCppProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(result.LoadedModels) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.LoadedModels))
	}
	if result.LoadedModels[0].Name != "llama-3.1-8b-instruct" {
		t.Errorf("unexpected model name: %s", result.LoadedModels[0].Name)
	}
	if result.VRAMUsedMB != 0 {
		t.Errorf("expected VRAMUsedMB=0 for llama.cpp, got %d", result.VRAMUsedMB)
	}
}

// TestLlamaCppProbe_RootDigest_WhenReported (P406): llama.cpp's OpenAI-
// compatible /v1/models is hand-rolled and not guaranteed to include a
// "root" field on every version - this test only locks in that it's parsed
// opportunistically when present, not that it's universally available.
func TestLlamaCppProbe_RootDigest_WhenReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama-3.1-8b-instruct", "root": "/gguf/llama-3.1-8b-instruct.Q4_K_M.gguf"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &LlamaCppProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "/gguf/llama-3.1-8b-instruct.Q4_K_M.gguf" {
		t.Errorf("expected Digest populated from root, got %q", result.LoadedModels[0].Digest)
	}
}

func TestLlamaCppProbe_HealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := &LlamaCppProbe{client: testClient()}
	_, err := probe.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503 /health, got nil")
	}
}

// --- MLXProbe tests ---

func TestMLXProbe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "mlx-community/Llama-3.2-3B-Instruct-4bit"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &MLXProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(result.LoadedModels) != 1 {
		t.Fatalf("expected 1 model, got %d", len(result.LoadedModels))
	}
	if result.LoadedModels[0].Name != "mlx-community/Llama-3.2-3B-Instruct-4bit" {
		t.Errorf("unexpected model name: %s", result.LoadedModels[0].Name)
	}
	if result.VRAMUsedMB != 0 {
		t.Errorf("expected VRAMUsedMB=0 for mlx, got %d", result.VRAMUsedMB)
	}
}

// TestMLXProbe_RootDigest_WhenReported (P406): mlx_lm.server's minimal
// OpenAI-compatible surface is not guaranteed to include a "root" field -
// this only locks in opportunistic parsing when present.
func TestMLXProbe_RootDigest_WhenReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama-3-70b", "root": "mlx-community/Llama-3-70B-4bit"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	probe := &MLXProbe{client: testClient()}
	result, err := probe.Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.LoadedModels[0].Digest != "mlx-community/Llama-3-70B-4bit" {
		t.Errorf("expected Digest populated from root, got %q", result.LoadedModels[0].Digest)
	}
}

func TestMLXProbe_ModelsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := &MLXProbe{client: testClient()}
	_, err := probe.Probe(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 503 /v1/models, got nil")
	}
}

// --- NewProbe factory tests ---

func TestNewProbe_KnownRuntimes(t *testing.T) {
	client := testClient()
	tests := []struct {
		runtime string
		want    string
	}{
		{"ollama", "*runtime.OllamaProbe"},
		{"", "*runtime.OllamaProbe"},
		{"vllm", "*runtime.VLLMProbe"},
		{"tgi", "*runtime.TGIProbe"},
		{"llamacpp", "*runtime.LlamaCppProbe"},
		{"mlx", "*runtime.MLXProbe"},
	}
	for _, tt := range tests {
		p := NewProbe(tt.runtime, client)
		got := fmt.Sprintf("%T", p)
		if got != tt.want {
			t.Errorf("NewProbe(%q) = %s, want %s", tt.runtime, got, tt.want)
		}
	}
}

func TestNewProbe_UnknownRuntime_ReturnsOllamaProbe(t *testing.T) {
	// Must not panic; must return a usable OllamaProbe.
	probe := NewProbe("totally-unknown-runtime", testClient())
	if probe == nil {
		t.Fatal("NewProbe(unknown) returned nil")
	}
	_, ok := probe.(*OllamaProbe)
	if !ok {
		t.Errorf("expected *OllamaProbe for unknown runtime, got %T", probe)
	}
}
