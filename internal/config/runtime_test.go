package config

import (
	"strings"
	"testing"
)

func TestNodeRuntimeDefaultsToOllama(t *testing.T) {
	var cfg Config
	cfg.Nodes = []NodeConfig{
		{Name: "n", URL: "http://localhost:11434"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Nodes[0].Runtime != "ollama" {
		t.Errorf("runtime = %q, want \"ollama\"", cfg.Nodes[0].Runtime)
	}
}

func TestNodeRuntimeVllmAccepted(t *testing.T) {
	var cfg Config
	cfg.Nodes = []NodeConfig{
		{Name: "n", URL: "http://localhost:8000", Runtime: "vllm"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("runtime vllm should be accepted: %v", err)
	}
	if cfg.Nodes[0].Runtime != "vllm" {
		t.Errorf("runtime = %q, want \"vllm\"", cfg.Nodes[0].Runtime)
	}
}

func TestNodeRuntimeTgiAccepted(t *testing.T) {
	var cfg Config
	cfg.Nodes = []NodeConfig{
		{Name: "n", URL: "http://localhost:8080", Runtime: "tgi"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("runtime tgi should be accepted: %v", err)
	}
	if cfg.Nodes[0].Runtime != "tgi" {
		t.Errorf("runtime = %q, want \"tgi\"", cfg.Nodes[0].Runtime)
	}
}

func TestNodeRuntimeLlamacppAccepted(t *testing.T) {
	var cfg Config
	cfg.Nodes = []NodeConfig{
		{Name: "n", URL: "http://localhost:8080", Runtime: "llamacpp"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("runtime llamacpp should be accepted: %v", err)
	}
	if cfg.Nodes[0].Runtime != "llamacpp" {
		t.Errorf("runtime = %q, want \"llamacpp\"", cfg.Nodes[0].Runtime)
	}
}

func TestNodeRuntimeUnknownReturnsError(t *testing.T) {
	var cfg Config
	cfg.Nodes = []NodeConfig{
		{Name: "n", URL: "http://localhost:8080", Runtime: "unknown-backend"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for unknown runtime, got nil")
	}
	if !strings.Contains(err.Error(), "unknown runtime") {
		t.Errorf("error %q does not contain \"unknown runtime\"", err.Error())
	}
}
