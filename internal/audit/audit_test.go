package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogWritesJSONLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	e := Entry{
		Time:      time.Now().UTC(),
		RequestID: "abc123",
		KeyName:   "dev-key",
		Model:     "llama3.2:8b",
		Node:      "gpu-0",
		Status:    "warm",
		LatencyMs: 42,
		Cloud:     false,
	}
	l.Log(e)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var got Entry
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&got); err != nil {
		t.Fatalf("decode log entry: %v", err)
	}
	if got.RequestID != "abc123" {
		t.Errorf("request_id = %q, want abc123", got.RequestID)
	}
	if got.Model != "llama3.2:8b" {
		t.Errorf("model = %q, want llama3.2:8b", got.Model)
	}
	if got.LatencyMs != 42 {
		t.Errorf("latency_ms = %d, want 42", got.LatencyMs)
	}
	if got.Cloud {
		t.Error("cloud = true, want false")
	}
}

func TestLogNoopWhenDisabled(t *testing.T) {
	l, err := New("") // empty path = disabled
	if err != nil {
		t.Fatalf("New empty: %v", err)
	}
	// Must not panic.
	l.Log(Entry{RequestID: "x"})
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestLogMultipleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 5 {
		l.Log(Entry{RequestID: string(rune('A' + i)), LatencyMs: i * 10})
	}
	l.Close()

	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Errorf("line %d: %v", count, err)
		}
	}
	if count != 5 {
		t.Errorf("lines = %d, want 5", count)
	}
}
