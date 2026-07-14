package store_test

import (
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func float64p(v float64) *float64 { return &v }
func intp(v int) *int             { return &v }

// TestModelConfigCRUDAndPersistence verifies Set/Get/Delete round-trip a full
// profile and that it survives a reopen (item #20).
func TestModelConfigCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/mc.db"

	s := openTestDBAt(t, path)
	cfg := store.ModelConfig{
		Model:       "llama3.3:70b",
		NumCtx:      intp(8192),
		Temperature: float64p(0.6),
		TopP:        float64p(0.95),
		Stop:        []string{"</s>", "\n\n"},
		RPM:         intp(60),
	}
	if err := s.SetModelConfig(cfg); err != nil {
		t.Fatalf("SetModelConfig: %v", err)
	}

	got, err := s.GetModelConfig("llama3.3:70b")
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
	got2, err := s2.GetModelConfig("llama3.3:70b")
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
	if len(all) != 1 || all[0].Model != "llama3.3:70b" {
		t.Fatalf("AllModelConfigs = %+v", all)
	}

	if err := s2.DeleteModelConfig("llama3.3:70b"); err != nil {
		t.Fatalf("DeleteModelConfig: %v", err)
	}
	if _, err := s2.GetModelConfig("llama3.3:70b"); err != store.ErrNotFound {
		t.Fatalf("GetModelConfig after delete = %v, want ErrNotFound", err)
	}
}

// TestGetModelConfigNotFound verifies an unconfigured model reports
// ErrNotFound rather than a zero-value profile silently masquerading as
// "configured with all defaults".
func TestGetModelConfigNotFound(t *testing.T) {
	s := openTestDB(t)
	if _, err := s.GetModelConfig("never-configured"); err != store.ErrNotFound {
		t.Fatalf("GetModelConfig = %v, want ErrNotFound", err)
	}
}
