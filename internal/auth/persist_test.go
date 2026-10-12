package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// memCounterStore is an in-memory store for counter persistence tests. Only
// the counter methods are implemented; the embedded NopStore covers the rest.
type memCounterStore struct {
	store.NopStore
	saved  map[string]store.KeyCounterSnapshot
	failOn map[string]bool
}

func newMemCounterStore() *memCounterStore {
	return &memCounterStore{saved: map[string]store.KeyCounterSnapshot{}, failOn: map[string]bool{}}
}

func (s *memCounterStore) SaveKeyCounters(name string, snap store.KeyCounterSnapshot) error {
	if s.failOn[name] {
		return errors.New("disk full")
	}
	s.saved[name] = snap
	return nil
}

func (s *memCounterStore) AllKeyCounters() (map[string]store.KeyCounterSnapshot, error) {
	out := make(map[string]store.KeyCounterSnapshot, len(s.saved))
	for k, v := range s.saved {
		out[k] = v
	}
	return out, nil
}

func counterCfg() config.AuthConfig {
	return config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "a", Key: "sk-a", RateLimit: 1000},
			{Name: "b", Key: "sk-b", RateLimit: 1000},
			{Name: "c", Key: "sk-c", RateLimit: 1000},
		},
	}
}

// The startup ordering regression (counters restored only after the keys are
// registered) is pinned by the startup test in the main package.
func TestSaveLoadStateRoundTrip(t *testing.T) {
	mw := NewMiddleware(counterCfg())
	mw.byName["a"].counter.today, mw.byName["a"].counter.month, mw.byName["a"].counter.tokensMonth = 3, 7, 900
	mw.byName["b"].counter.today, mw.byName["b"].counter.month, mw.byName["b"].counter.tokensMonth = 1, 2, 50

	st := newMemCounterStore()
	if err := mw.SaveToStore(st); err != nil {
		t.Fatalf("SaveToStore: %v", err)
	}

	mw2 := NewMiddleware(counterCfg())
	if err := mw2.LoadFromStore(st); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		t1, m1, k1, _, _, _, _, _ := mw.KeyStats(name)
		t2, m2, k2, _, _, _, _, _ := mw2.KeyStats(name)
		if t1 != t2 || m1 != m2 || k1 != k2 {
			t.Errorf("%s stats after restore = (%d,%d,%d), want (%d,%d,%d)", name, t2, m2, k2, t1, m1, k1)
		}
	}
	if today, month, tok, _, _, _, _, _ := mw2.KeyStats("a"); today != 3 || month != 7 || tok != 900 {
		t.Errorf("a restored = (%d,%d,%d), want (3,7,900)", today, month, tok)
	}
}

func TestSaveToStoreContinuesPastFailingKey(t *testing.T) {
	mw := NewMiddleware(counterCfg())
	for _, n := range []string{"a", "b", "c"} {
		mw.byName[n].counter.today = 5
	}
	st := newMemCounterStore()
	st.failOn["b"] = true
	err := mw.SaveToStore(st)
	if err == nil {
		t.Fatal("expected an error for the failing key")
	}
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("error %q does not name the failing key", err)
	}
	for _, n := range []string{"a", "c"} {
		if _, ok := st.saved[n]; !ok {
			t.Errorf("key %q was not saved after another key failed", n)
		}
	}
}
