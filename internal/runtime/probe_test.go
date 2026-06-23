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
