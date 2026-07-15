package store_test

import (
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func float64p(v float64) *float64 { return &v }
func intp(v int) *int             { return &v }

// TestModelConfigCRUDAndPersistence verifies Set/Get/Delete round-trip a full
// profile and that it survives a reopen (item #20). Keyed by (model, node).
func TestModelConfigCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/mc.db"

	s := openTestDBAt(t, path)
	cfg := store.ModelConfig{
		Model:       "llama3.3:70b",
		Node:        "gpu-node-01",
		NumCtx:      intp(8192),
		Temperature: float64p(0.6),
		TopP:        float64p(0.95),
		Stop:        []string{"</s>", "\n\n"},
		RPM:         intp(60),
	}
	if err := s.SetModelConfig(cfg); err != nil {
		t.Fatalf("SetModelConfig: %v", err)
	}

	got, err := s.GetModelConfig("llama3.3:70b", "gpu-node-01")
	if err != nil {
		t.Fatalf("GetModelConfig: %v", err)
	}
	if got.NumCtx == nil || *got.NumCtx != 8192 {
		t.Fatalf("NumCtx = %v, want 8192", got.NumCtx)
	}
	if got.Temperature == nil || *got.Temperature != 0.6 {
		t.Fatalf("Temperature = %v, want 0.6", got.Temperature)
	}
	if len(got.Stop) != 2 || got.Stop[0] != "</s>" {
		t.Fatalf("Stop = %v", got.Stop)
	}
	// A field never set must stay nil, not a fabricated default (R1).
	if got.NumGPU != nil {
		t.Fatalf("NumGPU = %v, want nil (never configured)", got.NumGPU)
	}
	s.Close()

	// Survives reopen.
	s2 := openTestDBAt(t, path)
	defer s2.Close()
	got2, err := s2.GetModelConfig("llama3.3:70b", "gpu-node-01")
	if err != nil {
		t.Fatalf("GetModelConfig after reopen: %v", err)
	}
	if got2.RPM == nil || *got2.RPM != 60 {
		t.Fatalf("RPM after reopen = %v, want 60", got2.RPM)
	}

	all, err := s2.AllModelConfigs()
	if err != nil {
		t.Fatalf("AllModelConfigs: %v", err)
	}
	if len(all) != 1 || all[0].Model != "llama3.3:70b" || all[0].Node != "gpu-node-01" {
		t.Fatalf("AllModelConfigs = %+v", all)
	}

	if err := s2.DeleteModelConfig("llama3.3:70b", "gpu-node-01"); err != nil {
		t.Fatalf("DeleteModelConfig: %v", err)
	}
	if _, err := s2.GetModelConfig("llama3.3:70b", "gpu-node-01"); err != store.ErrNotFound {
		t.Fatalf("GetModelConfig after delete = %v, want ErrNotFound", err)
	}
}

// TestGetModelConfigNotFound verifies an unconfigured (model, node) pair
// reports ErrNotFound rather than a zero-value profile silently masquerading
// as "configured with all defaults".
func TestGetModelConfigNotFound(t *testing.T) {
	s := openTestDB(t)
	if _, err := s.GetModelConfig("never-configured", "gpu-node-01"); err != store.ErrNotFound {
		t.Fatalf("GetModelConfig = %v, want ErrNotFound", err)
	}
}

// TestModelConfigSameModelDifferentNodes verifies the same model name can
// carry two independent profiles on two different nodes - the core reason
// for keying by (model, node) rather than model alone (e.g. one Ollama node
// with a smaller VRAM budget wanting a smaller num_ctx than another node
// hosting the identical model, or two nodes running different runtimes
// entirely).
func TestModelConfigSameModelDifferentNodes(t *testing.T) {
	s := openTestDB(t)

	cfgA := store.ModelConfig{Model: "llama3.3:8b", Node: "gpu-node-01", NumCtx: intp(4096)}
	cfgB := store.ModelConfig{Model: "llama3.3:8b", Node: "gpu-node-02", NumCtx: intp(8192)}
	if err := s.SetModelConfig(cfgA); err != nil {
		t.Fatalf("SetModelConfig A: %v", err)
	}
	if err := s.SetModelConfig(cfgB); err != nil {
		t.Fatalf("SetModelConfig B: %v", err)
	}

	gotA, err := s.GetModelConfig("llama3.3:8b", "gpu-node-01")
	if err != nil {
		t.Fatalf("GetModelConfig A: %v", err)
	}
	if gotA.NumCtx == nil || *gotA.NumCtx != 4096 {
		t.Fatalf("node-01 NumCtx = %v, want 4096", gotA.NumCtx)
	}

	gotB, err := s.GetModelConfig("llama3.3:8b", "gpu-node-02")
	if err != nil {
		t.Fatalf("GetModelConfig B: %v", err)
	}
	if gotB.NumCtx == nil || *gotB.NumCtx != 8192 {
		t.Fatalf("node-02 NumCtx = %v, want 8192", gotB.NumCtx)
	}

	all, err := s.AllModelConfigs()
	if err != nil {
		t.Fatalf("AllModelConfigs: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllModelConfigs = %+v, want 2 rows for the same model on different nodes", all)
	}

	// Deleting node-01's profile must not touch node-02's.
	if err := s.DeleteModelConfig("llama3.3:8b", "gpu-node-01"); err != nil {
		t.Fatalf("DeleteModelConfig: %v", err)
	}
	if _, err := s.GetModelConfig("llama3.3:8b", "gpu-node-01"); err != store.ErrNotFound {
		t.Fatalf("node-01 after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.GetModelConfig("llama3.3:8b", "gpu-node-02"); err != nil {
		t.Fatalf("node-02 should be untouched: %v", err)
	}
}
