// Package audit provides structured audit logging for all proxy requests.
// Entries are written to SQLite via the store.Store interface.
package audit

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// Entry is one audit log record. Matches store.AuditEntry field for field.
type Entry struct {
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	KeyName       string    `json:"key_name"`
	Model         string    `json:"model"`
	Node          string    `json:"node"`
	Status        string    `json:"status"`
	LatencyMs     int       `json:"latency_ms"`
	Cloud         bool      `json:"cloud"`
	CloudModel    string    `json:"cloud_model,omitempty"`
	RoutingReason string    `json:"routing_reason,omitempty"`
}

// Logger writes audit entries to the store. Disabled (no-op) when enabled=false.
// enabled is an atomic.Bool, not a plain bool, because SetEnabled lets the
// admin Settings page flip audit logging on/off on a live process - the
// proxy's request-handling goroutines read it concurrently with that write.
type Logger struct {
	st      store.Store
	enabled atomic.Bool

	// writes is a bounded async queue so Log never blocks the proxy's
	// request-handling goroutine on a SQLite insert (this was a synchronous
	// per-request write before - see .local/audit-fixes-2026-08-03.md #1).
	writes    chan store.AuditEntry
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// lastAppendErrLog rate-limits the "audit append failed" log line (see
	// logAppendErr) - only run() touches it, so no lock is needed.
	lastAppendErrLog time.Time
}

// appendErrLogInterval bounds how often a persistently-failing
// AppendAuditLog is logged, so a sustained DB outage produces one line per
// interval instead of one per request.
const appendErrLogInterval = 30 * time.Second

// logAppendErr logs err at most once per appendErrLogInterval, so a
// persistently failing store write produces a visible, bounded log trail
// instead of either silence (the audit trail going dark with zero operator
// signal) or a line per dropped entry.
func (l *Logger) logAppendErr(err error) {
	now := time.Now()
	if now.Sub(l.lastAppendErrLog) < appendErrLogInterval {
		return
	}
	l.lastAppendErrLog = now
	log.Printf("audit logger: AppendAuditLog failed: %v", err)
}

// New returns a Logger backed by st. When enabled is false every Log call is
// a no-op, but Query still reads existing entries from the store.
func New(st store.Store, enabled bool) *Logger {
	l := &Logger{
		st:     st,
		writes: make(chan store.AuditEntry, 5000),
		done:   make(chan struct{}),
	}
	l.enabled.Store(enabled)
	l.wg.Add(1)
	go l.run()
	return l
}

func (l *Logger) run() {
	defer l.wg.Done()
	for {
		select {
		case e := <-l.writes:
			if err := l.st.AppendAuditLog(e); err != nil {
				l.logAppendErr(err)
			}
		case <-l.done:
			// Drain whatever is already buffered, then stop. writes is never
			// closed (Log keeps sending on it via a non-blocking select even
			// after Close), so this only races benignly: any entry enqueued
			// after the drain below just sits unread, it never panics on a
			// closed channel.
			for {
				select {
				case e := <-l.writes:
					if err := l.st.AppendAuditLog(e); err != nil {
						l.logAppendErr(err)
					}
				default:
					return
				}
			}
		}
	}
}

// SetEnabled flips audit logging on/off on a running Logger, so toggling the
// Settings page's audit_enabled control takes effect immediately instead of
// requiring a marbor restart.
func (l *Logger) SetEnabled(enabled bool) {
	if l == nil {
		return
	}
	l.enabled.Store(enabled)
}

// Log enqueues one audit entry for async persistence. No-ops if the logger
// is disabled. Never blocks the caller: if the write queue is completely
// backed up the entry is dropped and logged, same trade-off the admin
// package's request-log queue already makes.
func (l *Logger) Log(e Entry) {
	if l == nil || !l.enabled.Load() {
		return
	}
	entry := store.AuditEntry{
		Time:          e.Time,
		RequestID:     e.RequestID,
		KeyName:       e.KeyName,
		Model:         e.Model,
		Node:          e.Node,
		Status:        e.Status,
		LatencyMs:     e.LatencyMs,
		Cloud:         e.Cloud,
		CloudModel:    e.CloudModel,
		RoutingReason: e.RoutingReason,
	}
	select {
	case l.writes <- entry:
	default:
		log.Printf("audit logger: queue full, dropped audit entry for request %s", e.RequestID)
	}
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
			Time:          e.Time,
			RequestID:     e.RequestID,
			KeyName:       e.KeyName,
			Model:         e.Model,
			Node:          e.Node,
			Status:        e.Status,
			LatencyMs:     e.LatencyMs,
			Cloud:         e.Cloud,
			CloudModel:    e.CloudModel,
			RoutingReason: e.RoutingReason,
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

// Close drains any in-flight audit entries and stops the async writer. Call
// this after the HTTP servers have stopped accepting new requests and
// before closing the store, or the writer can still be writing through l.st
// after it's been closed. Safe to call more than once - main.go's
// os.Exit-before-defers restore path calls this explicitly, and the normal
// shutdown path calls it again via defer.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		close(l.done)
		l.wg.Wait()
	})
	return nil
}
