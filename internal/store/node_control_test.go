package store

import (
	"path/filepath"
	"testing"
)

func TestUpsertNodeControlDiscoveredThenGet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlDiscovered("gpu-1", "docker", "ollama", []string{"docker container \"ollama\" found"}); err != nil {
		t.Fatalf("UpsertNodeControlDiscovered: %v", err)
	}

	got, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if !found {
		t.Fatal("GetNodeControl: found=false, want true")
	}
	if got.Configured {
		t.Error("Configured=true after a bare discovery - want false until an explicit Accept")
	}
	if got.DiscoveredDriver != "docker" || got.DiscoveredIdentifier != "ollama" {
		t.Fatalf("Discovered = %+v, want docker/ollama", got)
	}
	if len(got.DiscoveredEvidence) != 1 {
		t.Fatalf("DiscoveredEvidence = %v, want 1 entry", got.DiscoveredEvidence)
	}
}

func TestUpsertNodeControlConfigured(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "systemd", "ollama.service", ""); err != nil {
		t.Fatalf("UpsertNodeControlConfigured: %v", err)
	}
	got, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if !found || !got.Configured || got.Driver != "systemd" || got.Identifier != "ollama.service" {
		t.Fatalf("GetNodeControl = %+v, found=%v, want configured systemd/ollama.service", got, found)
	}
}

// TestRescanNeverChangesConfigured is the single most important invariant
// this feature depends on (node-agent-capabilities.md section 5.6): a
// discovery re-scan must never silently switch what lifecycle actions
// read, even if it finds a different driver than what's currently accepted.
func TestRescanNeverChangesConfigured(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "systemd", "ollama.service", ""); err != nil {
		t.Fatalf("UpsertNodeControlConfigured: %v", err)
	}
	// Operator migrated to Docker last month; a re-scan now finds it.
	if err := st.UpsertNodeControlDiscovered("gpu-1", "docker", "ollama", []string{"docker container \"ollama\" found"}); err != nil {
		t.Fatalf("UpsertNodeControlDiscovered: %v", err)
	}

	got, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if !found {
		t.Fatal("found=false, want true")
	}
	if got.Driver != "systemd" || got.Identifier != "ollama.service" || !got.Configured {
		t.Fatalf("configured value changed after a re-scan: got %+v, want unchanged systemd/ollama.service/configured=true", got)
	}
	if got.DiscoveredDriver != "docker" || got.DiscoveredIdentifier != "ollama" {
		t.Fatalf("discovered = %+v, want the fresh docker/ollama scan result", got)
	}
}

func TestClearNodeControlConfiguredKeepsDiscovered(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "systemd", "ollama.service", ""); err != nil {
		t.Fatalf("UpsertNodeControlConfigured: %v", err)
	}
	if err := st.UpsertNodeControlDiscovered("gpu-1", "docker", "ollama", []string{"evidence"}); err != nil {
		t.Fatalf("UpsertNodeControlDiscovered: %v", err)
	}
	if err := st.ClearNodeControlConfigured("gpu-1"); err != nil {
		t.Fatalf("ClearNodeControlConfigured: %v", err)
	}

	got, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if !found {
		t.Fatal("found=false, want true (row still exists)")
	}
	if got.Configured || got.Driver != "" || got.Identifier != "" {
		t.Fatalf("expected configured cleared, got %+v", got)
	}
	if got.DiscoveredDriver != "docker" {
		t.Fatalf("expected discovered evidence preserved, got %+v", got)
	}
}

func TestUpsertNodeControlConfiguredWithStartCommand(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "process", "/var/run/ollama.pid", "/usr/local/bin/ollama serve"); err != nil {
		t.Fatalf("UpsertNodeControlConfigured: %v", err)
	}
	got, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if !found || got.StartCommand != "/usr/local/bin/ollama serve" {
		t.Fatalf("GetNodeControl = %+v, found=%v, want StartCommand preserved", got, found)
	}

	// ClearNodeControlConfigured must also clear StartCommand - it is part
	// of the accepted config, not the discovered evidence.
	if err := st.ClearNodeControlConfigured("gpu-1"); err != nil {
		t.Fatalf("ClearNodeControlConfigured: %v", err)
	}
	got, _, err = st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl after clear: %v", err)
	}
	if got.StartCommand != "" {
		t.Fatalf("expected StartCommand cleared, got %q", got.StartCommand)
	}
}

func TestGetNodeControlNotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	_, found, err := st.GetNodeControl("nonexistent")
	if err != nil {
		t.Fatalf("GetNodeControl: %v", err)
	}
	if found {
		t.Fatal("found=true for a node with no control row, want false")
	}
}

func TestAllNodeControl(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "systemd", "ollama.service", ""); err != nil {
		t.Fatalf("UpsertNodeControlConfigured(gpu-1): %v", err)
	}
	if err := st.UpsertNodeControlDiscovered("gpu-2", "docker", "ollama", nil); err != nil {
		t.Fatalf("UpsertNodeControlDiscovered(gpu-2): %v", err)
	}

	all, err := st.AllNodeControl()
	if err != nil {
		t.Fatalf("AllNodeControl: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllNodeControl returned %d rows, want 2", len(all))
	}
}

func TestDeleteNodeControl(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertNodeControlConfigured("gpu-1", "systemd", "ollama.service", ""); err != nil {
		t.Fatalf("UpsertNodeControlConfigured: %v", err)
	}
	if err := st.DeleteNodeControl("gpu-1"); err != nil {
		t.Fatalf("DeleteNodeControl: %v", err)
	}
	_, found, err := st.GetNodeControl("gpu-1")
	if err != nil {
		t.Fatalf("GetNodeControl after delete: %v", err)
	}
	if found {
		t.Fatal("found=true after DeleteNodeControl, want false")
	}
}
