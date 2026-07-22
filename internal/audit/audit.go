// Package audit provides structured audit logging for all proxy requests.
// Entries are written to SQLite via the store.Store interface.
package audit

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// Entry is one audit log record. Matches store.AuditEntry field for field.
type Entry struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	KeyName    string    `json:"key_name"`
	Model      string    `json:"model"`
	Node       string    `json:"node"`
	Status     string    `json:"status"`
	LatencyMs  int       `json:"latency_ms"`
	Cloud      bool      `json:"cloud"`
	CloudModel string    `json:"cloud_model,omitempty"`
}

// Logger writes audit entries to the store. Disabled (no-op) when enabled=false.
// enabled is an atomic.Bool, not a plain bool, because SetEnabled lets the
// admin Settings page flip audit logging on/off on a live process - the
// proxy's request-handling goroutines read it concurrently with that write.
type Logger struct {
	st      store.Store
	enabled atomic.Bool
}

// New returns a Logger backed by st. When enabled is false every Log call is
// a no-op, but Query still reads existing entries from the store.
func New(st store.Store, enabled bool) *Logger {
	l := &Logger{st: st}
	l.enabled.Store(enabled)
	return l
}

// SetEnabled flips audit logging on/off on a running Logger, so toggling the
// Settings page's audit_enabled control takes effect immediately instead of
// requiring a mesh restart.
func (l *Logger) SetEnabled(enabled bool) {
	if l == nil {
		return
	}
	l.enabled.Store(enabled)
}

// Log writes one audit entry. No-ops if the logger is disabled.
func (l *Logger) Log(e Entry) {
	if l == nil || !l.enabled.Load() {
		return
	}
	_ = l.st.AppendAuditLog(store.AuditEntry{
		Time:       e.Time,
		RequestID:  e.RequestID,
		KeyName:    e.KeyName,
		Model:      e.Model,
		Node:       e.Node,
		Status:     e.Status,
		LatencyMs:  e.LatencyMs,
		Cloud:      e.Cloud,
		CloudModel: e.CloudModel,
	})
}

// QueryOptions controls filtering and limiting for Query.
type QueryOptions struct {
	Limit          int
	Model          string
	Key            string
	Node           string
	StatusCategory string
	Cloud          *bool
	Since          time.Time
	Until          time.Time
}

// Query returns audit entries matching opts, newest first.
func (l *Logger) Query(opts QueryOptions) ([]Entry, error) {
	if l == nil {
		return []Entry{}, nil
	}
	raw, err := l.st.QueryAuditLog(store.AuditQuery{
		Limit:          opts.Limit,
		Model:          opts.Model,
		Key:            opts.Key,
		Node:           opts.Node,
		StatusCategory: opts.StatusCategory,
		Cloud:          opts.Cloud,
		Since:          opts.Since,
		Until:          opts.Until,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(raw))
	for _, e := range raw {
		out = append(out, Entry{
			Time:       e.Time,
			RequestID:  e.RequestID,
			KeyName:    e.KeyName,
			Model:      e.Model,
			Node:       e.Node,
			Status:     e.Status,
			LatencyMs:  e.LatencyMs,
			Cloud:      e.Cloud,
			CloudModel: e.CloudModel,
		})
	}
	return out, nil
}

// FilterModel returns true if the entry matches the model filter (case-insensitive substring).
// Kept for callers that filter in-process rather than via QueryOptions.
func FilterModel(entry Entry, model string) bool {
	if model == "" {
		return true
	}
	return strings.Contains(strings.ToLower(entry.Model), strings.ToLower(model))
}

// Close is a no-op. The underlying store manages its own lifecycle.
func (l *Logger) Close() error { return nil }
