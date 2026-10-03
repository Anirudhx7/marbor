// Package audit provides best-effort structured audit logging for requests that
// reach proxy completion handling (auth/policy rejections are not recorded).
// Entries are written to SQLite via the store.Store interface.
package audit

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
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
	// per-request write before).
	writes    chan store.AuditEntry
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// mu is the lifecycle lock. Log holds it for reading across its closed
	// check and its non-blocking send; Close takes it for writing only to flip
	// closed. It is a leaf lock: nothing holds it across a store call, a
	// channel receive, close(done) or wg.Wait, and the writer goroutine never
	// takes it.
	mu sync.RWMutex

	// lastAppendErrLog rate-limits the "audit append failed" log line (see
	// logAppendErr) - only run() touches it, so no lock is needed.
	lastAppendErrLog time.Time

	// dropped counts entries discarded because writes was full. Log is called
	// from many request goroutines, so the drop-warning throttle below is
	// atomic rather than lock-guarded; now and logf are swappable for tests.
	dropped         atomic.Uint64
	closed          atomic.Bool
	lastDropLogNano atomic.Int64
	now             func() time.Time
	logf            func(format string, args ...any)
}

// writeQueueSize bounds the async audit queue. When it is full, Log drops the
// entry rather than blocking the request path.
const writeQueueSize = 5000

// dropLogInterval bounds how often a full queue is reported, so sustained
// overload produces one warning per interval (carrying the cumulative count)
// instead of one line per dropped entry.
const dropLogInterval = 30 * time.Second

// Dropped returns the cumulative number of entries dropped because the write
// queue was full or the logger had already been closed.
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

// recordDrop counts a dropped entry and emits a rate-limited warning with the
// cumulative total.
func (l *Logger) recordDrop(reason string) {
	total := l.dropped.Add(1)
	metrics.AuditDropped()
	nowNano := l.now().UnixNano()
	last := l.lastDropLogNano.Load()
	if last != 0 && nowNano-last < int64(dropLogInterval) {
		return
	}
	if !l.lastDropLogNano.CompareAndSwap(last, nowNano) {
		return
	}
	l.logf("audit logger: %s, audit entries are being dropped (%d dropped since start)", reason, total)
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
		writes: make(chan store.AuditEntry, writeQueueSize),
		done:   make(chan struct{}),
		now:    time.Now,
		logf:   log.Printf,
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
			// Drain whatever is already buffered, then stop. Close flips
			// closed under the write lock before it closes done, and Log
			// checks closed and sends under the read lock, so every entry
			// that was accepted is already in writes by now and no further
			// send can succeed. Draining until empty is therefore final.
			// writes is never closed, so a late Log never panics.
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
	// The closed check and the send share one read lock, so Close cannot
	// finish its transition between them. Only the check and a non-blocking
	// send run under the lock; drops are counted after it is released.
	l.mu.RLock()
	if l.closed.Load() {
		l.mu.RUnlock()
		// Logging after Close is a lifecycle bug in the caller: nothing will
		// ever drain the queue. Count it like any other drop so it is visible
		// instead of silently piling up unread.
		l.recordDrop("entry logged after Close")
		return
	}
	select {
	case l.writes <- entry:
		l.mu.RUnlock()
	default:
		l.mu.RUnlock()
		l.recordDrop("write queue full")
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
// after it's been closed. Entries logged after Close are discarded and counted
// as drops (see Dropped). Safe to call more than once - main.go's
// os.Exit-before-defers restore path calls this explicitly, and the normal
// shutdown path calls it again via defer.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		// Waits for every in-flight Log (check plus send), then blocks later
		// ones. Released before the drain so a slow store write never stalls
		// request goroutines: they see closed and drop immediately.
		l.mu.Lock()
		l.closed.Store(true)
		l.mu.Unlock()
		close(l.done)
		l.wg.Wait()
	})
	return nil
}
