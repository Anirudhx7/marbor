package metrics

import (
	"fmt"
	"testing"
)

// TestBoundModelCardinality verifies the model label is capped so a client
// sending unbounded distinct model names cannot blow up Prometheus memory.
func TestBoundModelCardinality(t *testing.T) {
	if got := boundModel(""); got != "unknown" {
		t.Errorf(`boundModel("") = %q, want "unknown"`, got)
	}
	if got := boundModel("llama3.2:3b"); got != "llama3.2:3b" {
		t.Errorf("known model should pass through, got %q", got)
	}
	// A model already seen stays stable.
	if got := boundModel("llama3.2:3b"); got != "llama3.2:3b" {
		t.Errorf("repeat model should pass through, got %q", got)
	}
	// Flood well past the cap.
	for i := 0; i < maxModelLabels+100; i++ {
		boundModel(fmt.Sprintf("flood-model-%d", i))
	}
	// Cap reached: a brand-new model collapses to "other".
	if got := boundModel("brand-new-after-cap"); got != "other" {
		t.Errorf("past cap, new model = %q, want \"other\"", got)
	}
	// The seen set must never exceed the cap.
	modelLabelMu.Lock()
	n := len(seenModels)
	modelLabelMu.Unlock()
	if n > maxModelLabels {
		t.Errorf("seenModels grew to %d, want <= %d", n, maxModelLabels)
	}
}
