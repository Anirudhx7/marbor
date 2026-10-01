package router

import "testing"

func TestModelNamesEquivalent(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"llama3", "llama3", true},
		{"llama3", "llama3:latest", true},
		{"llama3:latest", "llama3", true},
		{"llama3", "llama3:8b", false},
		{"llama3:8b", "llama3:70b", false},
		{"llama3", "llama2", false},
		{"host:5000/ns/m", "host:5000/ns/m:latest", true},
		{"host:5000/ns/m:q4", "host:5000/ns/m:latest", false},
		{"host:5000/ns/m", "host:5000/ns/m", true},
		{"llama3@sha256:abc", "llama3", false},
		{"llama3", "llama3@sha256:abc", false},
		{"llama3@sha256:abc", "llama3@sha256:abc", true},
		{"Llama3", "llama3:latest", false},
		{" llama3", "llama3:latest", false},
	}
	for _, tt := range tests {
		if got := ModelNamesEquivalent(tt.a, tt.b); got != tt.want {
			t.Errorf("ModelNamesEquivalent(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestFindLoadedModel(t *testing.T) {
	models := []ModelInfo{{Name: "llama3:latest", Digest: "d1"}, {Name: "qwen:7b", Digest: "d2"}}
	if m, ok := findLoadedModel(models, "llama3"); !ok || m.Name != "llama3:latest" {
		t.Errorf("bare name: got %+v, %v; want the :latest entry", m, ok)
	}
	if m, ok := findLoadedModel(models, "qwen:7b"); !ok || m.Digest != "d2" {
		t.Errorf("exact name: got %+v, %v", m, ok)
	}
	if _, ok := findLoadedModel(models, "qwen"); ok {
		t.Error("qwen must not match qwen:7b")
	}
	// Exact match wins over an equivalent one listed earlier.
	both := []ModelInfo{{Name: "m:latest", Digest: "tagged"}, {Name: "m", Digest: "bare"}}
	if got, _ := findLoadedModel(both, "m"); got.Digest != "bare" {
		t.Errorf("exact match should win, got %+v", got)
	}
}

func TestWarmupSupportedForRuntime(t *testing.T) {
	for rt, want := range map[string]bool{
		"": true, "ollama": true, "vllm": false, "tgi": false, "llamacpp": false, "mlx": false,
	} {
		if got := WarmupSupportedForRuntime(rt); got != want {
			t.Errorf("WarmupSupportedForRuntime(%q) = %v, want %v", rt, got, want)
		}
	}
}
