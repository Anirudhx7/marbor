package audit

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// countingStore counts every entry that reaches the store.
type countingStore struct {
	store.NopStore
	n atomic.Int64
}

func (c *countingStore) AppendAuditLog(store.AuditEntry) error {
	c.n.Add(1)
	return nil
}

// countingBehindBlock holds each write until the embedded blockingStore is
// released, then counts the entry.
type countingBehindBlock struct {
	*blockingStore
	n atomic.Int64
}

func (c *countingBehindBlock) AppendAuditLog(e store.AuditEntry) error {
	_ = c.blockingStore.AppendAuditLog(e)
	c.n.Add(1)
	return nil
}

func TestCloseIsIdempotent(t *testing.T) {
	l := New(store.NopStore{}, true)
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		l2 := New(store.NopStore{}, true)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = l2.Close() }()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close calls deadlocked")
	}
}

func TestCloseDrainsQueuedEntries(t *testing.T) {
	const n = 200
	bs := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	cs := &countingBehindBlock{blockingStore: bs}
	l := New(cs, true)

	for i := 0; i < n; i++ {
		l.Log(Entry{RequestID: fmt.Sprintf("e%d", i)})
	}
	<-bs.entered // the writer holds the first entry; the rest sit in the queue
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(bs.release)
	}()
	l.Close() // must wait for the writer and drain the queue

	if got := cs.n.Load(); got != n {
		t.Fatalf("store received %d entries, want %d", got, n)
	}
	if got := l.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

func TestPostCloseWarningIsRateLimited(t *testing.T) {
	l := New(store.NopStore{}, true)
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
	lineCount := func() int { mu.Lock(); defer mu.Unlock(); return len(lines) }
	l.Close()

	for i := 0; i < 100; i++ {
		l.Log(Entry{RequestID: "late"})
	}
	if got := lineCount(); got != 1 {
		t.Fatalf("warnings within interval = %d, want 1", got)
	}

	mu.Lock()
	clock = clock.Add(dropLogInterval + time.Second)
	mu.Unlock()
	l.Log(Entry{RequestID: "late"})
	if got := lineCount(); got != 2 {
		t.Fatalf("warnings after interval = %d, want 2", got)
	}
	mu.Lock()
	second := lines[1]
	mu.Unlock()
	if !strings.Contains(second, "(101 dropped since start)") {
		t.Fatalf("second warning = %q, want cumulative count 101", second)
	}
	if got := l.Dropped(); got != 101 {
		t.Fatalf("Dropped() = %d, want 101 (every post-Close entry counted)", got)
	}
}

// Every entry handed to Log while Close runs must end up either in the store
// or in the drop counter.
func TestConcurrentLogAndCloseAccountsForEveryEntry(t *testing.T) {
	const (
		iterations = 300
		goroutines = 8
		perG       = 25
	)
	for it := 0; it < iterations; it++ {
		cs := &countingStore{}
		l := New(cs, true)
		l.logf = func(string, ...any) {}

		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < perG; i++ {
					l.Log(Entry{RequestID: "x"})
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.Close()
		}()
		close(start)
		wg.Wait()
		l.Close()

		submitted := int64(goroutines * perG)
		persisted := cs.n.Load()
		dropped := int64(l.Dropped())
		if submitted != persisted+dropped {
			t.Fatalf("iteration %d: submitted=%d persisted=%d dropped=%d: %d entries neither persisted nor counted",
				it, submitted, persisted, dropped, submitted-persisted-dropped)
		}
	}
}

// Close must wait for a Log call that is between its closed check and its
// send. The test stands in for that Log call by holding the lifecycle read
// lock itself: Close cannot return while the lock is held, and an entry sent
// before the lock is released must be persisted by the drain.
func TestCloseWaitsForInFlightEnqueue(t *testing.T) {
	cs := &countingStore{}
	l := New(cs, true)
	l.logf = func(string, ...any) {}

	l.mu.RLock()
	closeReturned := make(chan struct{})
	go func() {
		defer close(closeReturned)
		l.Close()
	}()

	select {
	case <-closeReturned:
		l.mu.RUnlock()
		t.Fatal("Close returned while an enqueue was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	l.writes <- store.AuditEntry{RequestID: "in-flight"}
	l.mu.RUnlock()

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the in-flight enqueue finished")
	}
	if got := cs.n.Load(); got != 1 {
		t.Fatalf("persisted = %d, want 1: the entry accepted before Close must be drained", got)
	}
	if got := l.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

// While Close waits for a slow store write, request goroutines must not be
// held up: Log returns promptly and counts a post-Close drop.
func TestLogDoesNotBlockWhileCloseDrains(t *testing.T) {
	bs := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	cs := &countingBehindBlock{blockingStore: bs}
	l := New(cs, true)
	l.logf = func(string, ...any) {}

	l.Log(Entry{RequestID: "queued"})
	<-bs.entered // the writer is now stuck inside the store

	closeReturned := make(chan struct{})
	go func() {
		defer close(closeReturned)
		l.Close()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !l.closed.Load() {
		if time.Now().After(deadline) {
			close(bs.release)
			t.Fatal("Close never marked the logger closed")
		}
		time.Sleep(time.Millisecond)
	}

	const late = 50
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		for i := 0; i < late; i++ {
			l.Log(Entry{RequestID: "late"})
		}
	}()
	select {
	case <-logged:
	case <-time.After(2 * time.Second):
		close(bs.release)
		t.Fatal("Log blocked while Close was waiting for the writer")
	}
	select {
	case <-closeReturned:
		t.Fatal("Close returned before the stuck store write finished")
	default:
	}

	close(bs.release)
	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the store was released")
	}
	if got := cs.n.Load(); got != 1 {
		t.Fatalf("persisted = %d, want 1", got)
	}
	if got := l.Dropped(); got != late {
		t.Fatalf("Dropped() = %d, want %d", got, late)
	}
}

// reentrantStore calls Log from inside AppendAuditLog, after Close has begun.
type reentrantStore struct {
	store.NopStore
	logger  atomic.Pointer[Logger]
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *reentrantStore) AppendAuditLog(store.AuditEntry) error {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
		r.logger.Load().Log(Entry{RequestID: "from-store"})
	})
	return nil
}

func TestStoreCallingLogDuringDrainDoesNotDeadlock(t *testing.T) {
	rs := &reentrantStore{entered: make(chan struct{}), release: make(chan struct{})}
	l := New(rs, true)
	l.logf = func(string, ...any) {}
	rs.logger.Store(l)

	l.Log(Entry{RequestID: "first"})
	<-rs.entered

	closeReturned := make(chan struct{})
	go func() {
		defer close(closeReturned)
		l.Close()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !l.closed.Load() {
		if time.Now().After(deadline) {
			close(rs.release)
			t.Fatal("Close never marked the logger closed")
		}
		time.Sleep(time.Millisecond)
	}
	close(rs.release)

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked when the store called Log during the drain")
	}
	if got := l.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1 (the Log call made after Close began)", got)
	}
}
