package admin

import (
	"context"
	"testing"

	"github.com/Anirudhx7/marbor/internal/router"
)

func TestNodeHasModel_BareNameMatchesLatestOnOllama(t *testing.T) {
	for _, runtime := range []string{"ollama", ""} {
		s := newBenchTestServer(t, "http://127.0.0.1:1", "", "llama3:latest")
		for _, n := range s.router.Nodes() {
			n.Lock()
			n.Runtime = runtime
			n.Unlock()
		}
		if !s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
			t.Errorf("runtime %q: bare name should match the resident :latest copy", runtime)
		}
		if !s.nodeHasModel(context.Background(), "gpu-0", "llama3:latest") {
			t.Errorf("runtime %q: exact name should match", runtime)
		}
		if s.nodeHasModel(context.Background(), "gpu-0", "llama3:8b") {
			t.Errorf("runtime %q: a different tag must not match", runtime)
		}
	}
}

func TestNodeHasModel_NonOllamaStaysExact(t *testing.T) {
	s := newBenchTestServer(t, "http://127.0.0.1:1", "", "llama3:latest")
	for _, n := range s.router.Nodes() {
		n.Lock()
		n.Runtime = "vllm"
		n.Unlock()
	}
	if s.nodeHasModel(context.Background(), "gpu-0", "llama3") {
		t.Error("vllm: bare name must not match a :latest id")
	}
	if !s.nodeHasModel(context.Background(), "gpu-0", "llama3:latest") {
		t.Error("vllm: exact id should match")
	}
}

func TestModelNamesMatchOnRuntime_Exported(t *testing.T) {
	if !router.ModelNamesMatchOnRuntime("ollama", "a", "a:latest") {
		t.Error("ollama should treat bare and :latest as one model")
	}
	if router.ModelNamesMatchOnRuntime("llama.cpp", "a", "a:latest") {
		t.Error("llama.cpp should be exact")
	}
}
