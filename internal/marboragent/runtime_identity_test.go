package marboragent

import "testing"

// TestRuntimeRegistryPortMatchReusesID verifies a runtime restarting on the
// same port keeps its RuntimeID.
func TestRuntimeRegistryPortMatchReusesID(t *testing.T) {
	reg := &runtimeRegistry{}
	first := reg.Reconcile([]DetectedRuntime{{Name: "ollama", URL: "http://localhost:11434", Port: 11434}})
	if first[0].ID == "" {
		t.Fatal("expected a non-empty RuntimeID on first detection")
	}

	second := reg.Reconcile([]DetectedRuntime{{Name: "ollama", URL: "http://localhost:11434", Port: 11434}})
	if second[0].ID != first[0].ID {
		t.Errorf("RuntimeID changed across cycles at the same port: %q -> %q", first[0].ID, second[0].ID)
	}
}

// TestRuntimeRegistryPortMoveKeepsIDForSingleInstance is the specific case
// the user called out: a lone instance of a runtime type moving to a new
// port must keep its identity, not mint a new one.
func TestRuntimeRegistryPortMoveKeepsIDForSingleInstance(t *testing.T) {
	reg := &runtimeRegistry{}
	first := reg.Reconcile([]DetectedRuntime{{Name: "vllm", URL: "http://localhost:8000", Port: 8000}})
	id := first[0].ID

	moved := reg.Reconcile([]DetectedRuntime{{Name: "vllm", URL: "http://localhost:8001", Port: 8001}})
	if moved[0].ID != id {
		t.Errorf("RuntimeID changed after a port move for a lone instance: %q -> %q", id, moved[0].ID)
	}
	if moved[0].Port != 8001 {
		t.Errorf("Port = %d, want 8001 (attribute updates even though identity doesn't)", moved[0].Port)
	}
}

// TestRuntimeRegistryMultiRuntimeAssignsDistinctIDs verifies two different
// runtime types on one host each get their own stable identity.
func TestRuntimeRegistryMultiRuntimeAssignsDistinctIDs(t *testing.T) {
	reg := &runtimeRegistry{}
	out := reg.Reconcile([]DetectedRuntime{
		{Name: "ollama", URL: "http://localhost:11434", Port: 11434},
		{Name: "vllm", URL: "http://localhost:8000", Port: 8000},
	})
	if out[0].ID == "" || out[1].ID == "" || out[0].ID == out[1].ID {
		t.Fatalf("expected two distinct non-empty RuntimeIDs, got %q and %q", out[0].ID, out[1].ID)
	}

	// A further cycle with both still present, in the same order, must keep
	// both IDs stable.
	again := reg.Reconcile([]DetectedRuntime{
		{Name: "ollama", URL: "http://localhost:11434", Port: 11434},
		{Name: "vllm", URL: "http://localhost:8000", Port: 8000},
	})
	if again[0].ID != out[0].ID || again[1].ID != out[1].ID {
		t.Errorf("IDs changed across a stable cycle: (%q,%q) -> (%q,%q)", out[0].ID, out[1].ID, again[0].ID, again[1].ID)
	}
}

// TestRuntimeRegistryAmbiguousSameTypeMoveStillAssignsIDs documents the known
// heuristic limit (see plan's Scope section): when two same-typed instances
// both change ports in the same cycle, the registry can't disambiguate which
// is which, but each must still end up with *some* stable non-empty ID
// rather than an error or a shared ID.
func TestRuntimeRegistryAmbiguousSameTypeMoveStillAssignsIDs(t *testing.T) {
	reg := &runtimeRegistry{}
	reg.Reconcile([]DetectedRuntime{
		{Name: "vllm", URL: "http://localhost:8000", Port: 8000},
		{Name: "vllm", URL: "http://localhost:8001", Port: 8001},
	})

	moved := reg.Reconcile([]DetectedRuntime{
		{Name: "vllm", URL: "http://localhost:9000", Port: 9000},
		{Name: "vllm", URL: "http://localhost:9001", Port: 9001},
	})
	if moved[0].ID == "" || moved[1].ID == "" {
		t.Fatalf("expected both ambiguous same-type instances to still get a non-empty ID, got %q and %q", moved[0].ID, moved[1].ID)
	}
	if moved[0].ID == moved[1].ID {
		t.Errorf("two distinct runtime instances must not share a RuntimeID: both got %q", moved[0].ID)
	}
}

// TestRuntimeRegistryStaleEntryNotPrunedImmediately verifies a runtime that
// disappears for one cycle doesn't lose its registry entry (avoids identity
// churn from one transient probe miss) - reappearing at the same port later
// must still reuse the original ID.
func TestRuntimeRegistryStaleEntryNotPrunedImmediately(t *testing.T) {
	reg := &runtimeRegistry{}
	first := reg.Reconcile([]DetectedRuntime{{Name: "ollama", URL: "http://localhost:11434", Port: 11434}})
	id := first[0].ID

	// One cycle where nothing is detected (transient miss).
	reg.Reconcile(nil)

	again := reg.Reconcile([]DetectedRuntime{{Name: "ollama", URL: "http://localhost:11434", Port: 11434}})
	if again[0].ID != id {
		t.Errorf("RuntimeID = %q after a transient miss, want reused %q", again[0].ID, id)
	}
}
