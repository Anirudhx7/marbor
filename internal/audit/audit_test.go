package audit

import (
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func TestLogEnabled(t *testing.T) {
	st := store.NopStore{}
	l := New(st, true)
	// Must not panic; NopStore silently drops the entry.
	l.Log(Entry{
		Time:      time.Now().UTC(),
		RequestID: "abc123",
		KeyName:   "dev-key",
		Model:     "llama3.2:8b",
		Node:      "gpu-0",
		Status:    "warm",
		LatencyMs: 42,
		Cloud:     false,
	})
	l.Close()
}

func TestLogDisabled(t *testing.T) {
	st := store.NopStore{}
	l := New(st, false)
	// Must not panic when disabled.
	l.Log(Entry{RequestID: "x"})
	l.Close()
}
