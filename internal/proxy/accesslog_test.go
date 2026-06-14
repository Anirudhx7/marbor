package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestAccessLoggerWritesJSONLine(t *testing.T) {
	var buf bytes.Buffer
	logger := NewAccessLogger(&buf, true)

	entry := AccessLogEntry{
		RequestID:  "req-abc123",
		KeyName:    "my-key",
		Model:      "llama3",
		Node:       "node-1",
		Status:     200,
		LatencyMs:  42,
		Cloud:      false,
		TimeUnixMs: 1718000000000,
	}
	logger.Log(entry)

	out := buf.String()

	// Must end with exactly one newline.
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected output to end with newline, got: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one newline, got %d in: %q", strings.Count(out, "\n"), out)
	}

	// Must round-trip as valid JSON with correct field values.
	var got AccessLogEntry
	if err := json.Unmarshal([]byte(strings.TrimRight(out, "\n")), &got); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if got.Status != entry.Status {
		t.Errorf("Status: want %d, got %d", entry.Status, got.Status)
	}
	if got.KeyName != entry.KeyName {
		t.Errorf("KeyName: want %q, got %q", entry.KeyName, got.KeyName)
	}
	if got.Model != entry.Model {
		t.Errorf("Model: want %q, got %q", entry.Model, got.Model)
	}
	if got.LatencyMs != entry.LatencyMs {
		t.Errorf("LatencyMs: want %d, got %d", entry.LatencyMs, got.LatencyMs)
	}
	if got.Cloud != entry.Cloud {
		t.Errorf("Cloud: want %v, got %v", entry.Cloud, got.Cloud)
	}
}

func TestAccessLoggerDisabledWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := NewAccessLogger(&buf, false)

	logger.Log(AccessLogEntry{
		RequestID: "req-noop",
		KeyName:   "k",
		Model:     "m",
		Node:      "n",
		Status:    200,
		LatencyMs: 1,
		Cloud:     false,
	})

	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer when disabled, got %d bytes: %q", buf.Len(), buf.String())
	}
}

func TestAccessLoggerConcurrentLinesIntact(t *testing.T) {
	const goroutines = 50

	var buf bytes.Buffer
	logger := NewAccessLogger(&buf, true)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			logger.Log(AccessLogEntry{
				RequestID:  "req",
				KeyName:    "key",
				Model:      "model",
				Node:       "node",
				Status:     200,
				LatencyMs:  int64(n),
				Cloud:      false,
				TimeUnixMs: int64(n),
			})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(buf.String(), "\n")
	// The final element after splitting on '\n' is always an empty string
	// because every line is terminated with '\n'.
	nonEmpty := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		nonEmpty++
		var entry AccessLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line is not valid JSON (concurrent interleave?): %q - error: %v", line, err)
		}
	}

	if nonEmpty != goroutines {
		t.Errorf("expected %d non-empty lines, got %d", goroutines, nonEmpty)
	}
}
