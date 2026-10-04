package audit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Anirudhx7/marbor/internal/store"
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

// blockingStore stalls AppendAuditLog until release is closed, so tests can
// hold the writer goroutine and fill the queue deterministically.
type blockingStore struct {
	store.NopStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStore) AppendAuditLog(store.AuditEntry) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

func droppedMetric(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "marbor_audit_dropped_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("marbor_audit_dropped_total not registered")
	return 0
}

func TestLogQueueFullDropsCountsAndRateLimitsWarning(t *testing.T) {
	bs := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	l := New(bs, true)

	var (
		mu    sync.Mutex
		lines []string
		clock = time.Unix(1_700_000_000, 0)
	)
	l.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	l.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	lineCount := func() int { mu.Lock(); defer mu.Unlock(); return len(lines) }

	// First entry is taken by the writer, which then blocks; the next
	// writeQueueSize entries fill the queue without dropping.
	l.Log(Entry{RequestID: "first"})
	<-bs.entered
	for i := 0; i < writeQueueSize; i++ {
		l.Log(Entry{RequestID: "fill"})
	}
	if got := l.Dropped(); got != 0 {
		t.Fatalf("dropped = %d before queue overflow, want 0", got)
	}

	before := droppedMetric(t)
	for i := 0; i < 3; i++ {
		l.Log(Entry{RequestID: "overflow"})
	}
	if got := l.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3", got)
	}
	if got := droppedMetric(t) - before; got != 3 {
		t.Fatalf("metric delta = %v, want 3", got)
	}
	if got := lineCount(); got != 1 {
		t.Fatalf("warnings within interval = %d, want 1 (rate-limited)", got)
	}
	if !strings.Contains(lines[0], "(1 dropped since start)") {
		t.Fatalf("first warning = %q, want cumulative count 1", lines[0])
	}

	advance(dropLogInterval + time.Second)
	l.Log(Entry{RequestID: "overflow"})
	if got := lineCount(); got != 2 {
		t.Fatalf("warnings after interval = %d, want 2", got)
	}
	if !strings.Contains(lines[1], "(4 dropped since start)") {
		t.Fatalf("second warning = %q, want cumulative count 4", lines[1])
	}

	close(bs.release)
	l.Close()
}

func TestLogDisabledDoesNotCountDrops(t *testing.T) {
	l := New(store.NopStore{}, false)
	l.Log(Entry{RequestID: "x"})
	if got := l.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d on disabled logger, want 0", got)
	}
	l.Close()
}

func TestLogAfterCloseIsDiscardedAndCounted(t *testing.T) {
	l := New(store.NopStore{}, true)
	var lines []string
	l.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	l.Close()

	l.Log(Entry{RequestID: "late"})
	if got := l.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d after post-Close Log, want 1", got)
	}
	if n := len(l.writes); n != 0 {
		t.Fatalf("queue holds %d entries after post-Close Log, want 0", n)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "logged after Close") {
		t.Fatalf("warnings = %v, want one post-Close warning", lines)
	}
}

// A caller must never be held up by a full queue: Log has to return promptly
// even while the store is stalled and every queue slot is taken.
func TestLogQueueFullReturnsPromptly(t *testing.T) {
	bs := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	l := New(bs, true)
	l.logf = func(string, ...any) {}
	defer func() { close(bs.release); l.Close() }()

	l.Log(Entry{RequestID: "first"})
	<-bs.entered
	for i := 0; i < writeQueueSize; i++ {
		l.Log(Entry{RequestID: "fill"})
	}

	returned := make(chan struct{})
	go func() {
		l.Log(Entry{RequestID: "overflow"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Log blocked on a full queue")
	}
	if got := l.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
}

// scrapeDropped reads marbor_audit_dropped_total from the real exposition
// output, parsing the text body line by line.
func scrapeDropped(t *testing.T) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	const prefix = "marbor_audit_dropped_total "
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatal("marbor_audit_dropped_total line not found in exposition output")
	return 0
}

// The drop counter must show up on the metrics endpoint itself, not only in
// the registry. The registry is process-global, so compare a delta.
func TestAuditDroppedExposedOnMetricsEndpoint(t *testing.T) {
	before := scrapeDropped(t)

	l := New(store.NopStore{}, true)
	l.logf = func(string, ...any) {}
	l.Close()
	l.Log(Entry{RequestID: "late"}) // a post-Close Log is counted as a drop

	if after := scrapeDropped(t); after != before+1 {
		t.Fatalf("exposed marbor_audit_dropped_total = %v, want %v", after, before+1)
	}
}

// Drops caused by logging after Close feed the metric too, not just the
// logger's own counter.
func TestLogAfterCloseCountsMetric(t *testing.T) {
	l := New(store.NopStore{}, true)
	l.logf = func(string, ...any) {}
	l.Close()

	before := droppedMetric(t)
	for i := 0; i < 4; i++ {
		l.Log(Entry{RequestID: "late"})
	}
	if got := droppedMetric(t) - before; got != 4 {
		t.Fatalf("metric delta = %v after 4 post-Close Logs, want 4", got)
	}
	if got := l.Dropped(); got != 4 {
		t.Fatalf("Dropped() = %d, want 4", got)
	}
}
