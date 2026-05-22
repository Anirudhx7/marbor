// Package audit provides append-only structured audit logging for all proxy requests.
// Each entry is a JSON line written to a configurable file path.
// No sensitive data (API key values, request bodies) is logged — only metadata.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one audit log record. Written as a single JSON line.
type Entry struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	KeyName   string    `json:"key_name"`
	Model     string    `json:"model"`
	Node      string    `json:"node"`
	Status    string    `json:"status"`
	LatencyMs int       `json:"latency_ms"`
	Cloud     bool      `json:"cloud"`
}

// Logger writes audit entries to a file in JSON-lines format.
// Writes are serialized via mutex — safe for concurrent use.
// Entries are flushed to disk immediately (no buffering) for durability.
type Logger struct {
	mu   sync.Mutex
	file *os.File
}

// New opens (or creates) the audit log file at path for append-only writing.
// Returns a no-op logger if path is empty.
func New(path string) (*Logger, error) {
	if path == "" {
		return &Logger{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return nil, fmt.Errorf("audit log open %s: %w", path, err)
	}
	return &Logger{file: f}, nil
}

// Log writes one audit entry. Safe to call from multiple goroutines.
// No-ops if the logger has no file (disabled).
func (l *Logger) Log(e Entry) {
	if l == nil || l.file == nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	l.file.Write(b) //nolint:errcheck — best-effort audit write
	l.mu.Unlock()
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
