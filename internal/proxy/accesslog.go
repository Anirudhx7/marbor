package proxy

// accesslog.go - structured JSON access logger for the proxy layer.
// Each proxied request can be logged as a single JSON line to any io.Writer
// (file, stderr, etc.). The logger is concurrency-safe: concurrent requests
// never produce interleaved output. API key VALUES are never recorded - only
// the key name is carried in AccessLogEntry by design.

import (
	"encoding/json"
	"io"
	"sync"
)

// AccessLogEntry holds the per-request fields written to the access log.
type AccessLogEntry struct {
	RequestID  string `json:"request_id"`
	KeyName    string `json:"key_name"`
	Model      string `json:"model"`
	Node       string `json:"node"`
	Status     int    `json:"status"`
	LatencyMs  int64  `json:"latency_ms"`
	Cloud      bool   `json:"cloud"`
	TimeUnixMs int64  `json:"ts"`
}

// AccessLogger writes one JSON line per request to an underlying io.Writer.
// It is safe for concurrent use.
type AccessLogger struct {
	w       io.Writer
	enabled bool
	mu      sync.Mutex
}

// NewAccessLogger returns an AccessLogger that writes to w when enabled is
// true. Pass enabled=false to create a no-op logger (w may be nil in that
// case).
func NewAccessLogger(w io.Writer, enabled bool) *AccessLogger {
	return &AccessLogger{w: w, enabled: enabled}
}

// Log serialises e as a single JSON line followed by '\n' and writes it
// atomically under the mutex. If the logger is disabled or w is nil the call
// is a no-op. Write errors are silently swallowed - access logging must never
// break request handling.
func (a *AccessLogger) Log(e AccessLogEntry) {
	if !a.enabled || a.w == nil {
		return
	}

	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	// Append newline in the same allocation to guarantee a single Write call.
	b = append(b, '\n')

	a.mu.Lock()
	_, _ = a.w.Write(b) //nolint:errcheck
	a.mu.Unlock()
}
