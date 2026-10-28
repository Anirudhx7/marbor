package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

func openCloudTokensStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cloud-tokens.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func restartedServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	s := newTestServer()
	s.SetStore(st)
	if err := s.LoadFromStore(); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	return s
}

// A restart must not blank the persisted cloud spend: the token count that
// proves the spend was parsed from real responses has to survive with it.
func TestCloudSpendSurvivesRestart(t *testing.T) {
	st := openCloudTokensStore(t)
	s1 := newTestServer()
	s1.SetStore(st)
	s1.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 2000)
	s1.flushCounters()

	s2 := restartedServer(t, st)
	_, spent := s2.savingsUSD()
	if spent == nil {
		t.Fatal("spent is nil after restart, want the persisted cloud spend")
	}
	if got := spent.(float64); got < 0.0199 || got > 0.0201 {
		t.Fatalf("spent = %v, want 0.02", got)
	}
}

// A database written before cloud tokens were persisted has cloud requests but
// no token count. The spend must stay "-" (nil) rather than be guessed.
func TestCloudSpendNilWhenTokenCountNeverPersisted(t *testing.T) {
	st := openCloudTokensStore(t)
	if err := st.SetCounters(store.Counters{CloudRequests: 4, CloudSpentUSD: 1.5}); err != nil {
		t.Fatalf("SetCounters: %v", err)
	}
	s := restartedServer(t, st)
	if _, spent := s.savingsUSD(); spent != nil {
		t.Fatalf("spent = %v, want nil when no token count was ever persisted", spent)
	}
}

// A garbled, negative or empty saved count must not fail startup or invent a
// number: it is ignored and the spend stays "-".
func TestCloudTokensInvalidSavedValueIgnored(t *testing.T) {
	for _, bad := range []string{"abc", "-5", ""} {
		st := openCloudTokensStore(t)
		if err := st.SetCounters(store.Counters{CloudRequests: 4, CloudSpentUSD: 1.5}); err != nil {
			t.Fatalf("SetCounters: %v", err)
		}
		if err := st.SetSetting(cloudTokensSettingKey, bad); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
		s := restartedServer(t, st)
		if got := atomic.LoadInt64(&s.cloudTokens); got != 0 {
			t.Fatalf("saved %q: cloudTokens = %d, want 0", bad, got)
		}
		if _, spent := s.savingsUSD(); spent != nil {
			t.Fatalf("saved %q: spent = %v, want nil", bad, spent)
		}
	}
}

// The token key is only rewritten when the count changed since the last flush.
func TestCloudTokensFlushSkipsUnchangedValue(t *testing.T) {
	st := openCloudTokensStore(t)
	s := newTestServer()
	s.SetStore(st)
	s.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 100)
	s.flushCounters()
	// Change the stored value behind the server's back; an unchanged count
	// must not overwrite it.
	if err := st.SetSetting(cloudTokensSettingKey, "999"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	s.flushCounters()
	if v, _ := st.GetSetting(cloudTokensSettingKey); v != "999" {
		t.Fatalf("unchanged count rewrote the key: %q", v)
	}
	s.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 50)
	s.flushCounters()
	if v, _ := st.GetSetting(cloudTokensSettingKey); v != "150" {
		t.Fatalf("changed count not written: %q", v)
	}
}

// Tokens recorded after a restart add to the restored count.
func TestCloudTokensAccumulateAcrossRestart(t *testing.T) {
	st := openCloudTokensStore(t)
	s1 := newTestServer()
	s1.SetStore(st)
	s1.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 1000)
	s1.flushCounters()

	s2 := restartedServer(t, st)
	s2.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 500)
	s2.flushCounters()

	s3 := restartedServer(t, st)
	if got := atomic.LoadInt64(&s3.cloudTokens); got != 1500 {
		t.Fatalf("cloudTokens = %d, want 1500", got)
	}
}

// Tracking and flushing from many goroutines must be race-free (go test -race).
func TestCloudTokensConcurrentTrackAndFlush(t *testing.T) {
	st := openCloudTokensStore(t)
	s := newTestServer()
	s.SetStore(st)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 10)
				s.flushCounters()
			}
		}()
	}
	wg.Wait()
	s.flushCounters()
	if got := atomic.LoadInt64(&restartedServer(t, st).cloudTokens); got != 1600 {
		t.Fatalf("restored cloudTokens = %d, want 1600", got)
	}
}

func assertSinceUTC(t *testing.T, name string, body []byte, start time.Time) {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	since, _ := m["since"].(string)
	if since == "" {
		t.Fatalf("%s: since missing in %s", name, body)
	}
	want := start.UTC().Format(time.RFC3339)
	if since != want {
		t.Fatalf("%s: since = %q, want UTC %q", name, since, want)
	}
}

func TestSinceIsUTCOnAnalyticsAndSavings(t *testing.T) {
	s := newTestServer()
	// A non-UTC start time proves the handlers convert rather than rely on
	// the host zone being UTC.
	s.startTime = time.Date(2026, 10, 24, 9, 30, 0, 0, time.FixedZone("IST", 5*3600+1800))

	for name, h := range map[string]func(http.ResponseWriter, *http.Request){
		"analytics": s.handleAnalytics,
		"savings":   s.handleSavings,
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/admin/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, w.Code)
		}
		assertSinceUTC(t, name, w.Body.Bytes(), s.startTime)
	}
}

// failingStore fails the next SetSetting call once, then behaves normally.
type failingStore struct {
	store.Store
	failSetting atomic.Bool
}

func (f *failingStore) SetSetting(key, value string) error {
	if f.failSetting.CompareAndSwap(true, false) {
		return errors.New("injected write failure")
	}
	return f.Store.SetSetting(key, value)
}

// A failed token write must not advance the flush marker, and the value must
// be retried on the next flush.
func TestCloudTokensFlushRetriedAfterWriteFailure(t *testing.T) {
	fs := &failingStore{Store: openCloudTokensStore(t)}
	s := newTestServer()
	s.SetStore(fs)
	s.TrackCloudCostModel("k", "openai", "gpt-x", 0.01, 100)

	fs.failSetting.Store(true)
	s.flushCounters()
	if got := s.cloudTokensFlushed.Load(); got != 0 {
		t.Fatalf("marker advanced to %d after a failed write", got)
	}
	if _, err := fs.GetSetting(cloudTokensSettingKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("key written despite failure, err = %v", err)
	}

	s.flushCounters()
	if got := s.cloudTokensFlushed.Load(); got != 100 {
		t.Fatalf("marker = %d after retry, want 100", got)
	}
	if v, _ := fs.GetSetting(cloudTokensSettingKey); v != "100" {
		t.Fatalf("stored = %q after retry, want 100", v)
	}
}

// An invalid saved count is replaced by a clean "0" so it does not log again
// on every boot.
func TestCloudTokensInvalidSavedValueIsReset(t *testing.T) {
	st := openCloudTokensStore(t)
	if err := st.SetSetting(cloudTokensSettingKey, "garbage"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	restartedServer(t, st)
	if v, _ := st.GetSetting(cloudTokensSettingKey); v != "0" {
		t.Fatalf("stored = %q, want 0", v)
	}
}
