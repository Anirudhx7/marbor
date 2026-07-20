package store_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// TestOpenStampsCurrentSchemaVersion verifies a fresh DB ends up recording
// the binary's current schema version after Open().
func TestOpenStampsCurrentSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	got, err := s.GetSetting("schema_version")
	if err != nil {
		t.Fatalf("GetSetting(schema_version): %v", err)
	}
	want := strconv.Itoa(store.CurrentSchemaVersion)
	if got != want {
		t.Fatalf("schema_version = %q, want %q", got, want)
	}
}

// TestOpenRefusesNewerSchemaVersion verifies that a DB stamped with a schema
// version newer than the binary supports is refused at Open(), rather than
// silently migrated against.
func TestOpenRefusesNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	futureVersion := strconv.Itoa(store.CurrentSchemaVersion + 1)
	if err := s.SetSetting("schema_version", futureVersion); err != nil {
		t.Fatalf("SetSetting(schema_version): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := store.Open(path); err == nil {
		t.Fatal("Open: expected error reopening a DB with a newer schema_version, got nil")
	}
}
