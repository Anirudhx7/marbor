package auth

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// The store hands back LastReset in UTC. Rollover is decided in local time, so
// a restored counter must roll over at local midnight, not UTC midnight.
func TestLoadFromStoreRollsOverAtLocalMidnight(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC+10", 10*60*60)
	t.Cleanup(func() { time.Local = old })

	now := time.Now()
	localMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)

	load := func(lastReset time.Time) (today, month int) {
		mw := NewMiddleware(config.AuthConfig{
			Enabled: config.BoolPtr(true),
			Keys:    []config.KeyConfig{{Name: "k", Key: "sk-k", RateLimit: 10}},
		})
		st := newMemCounterStore()
		st.saved["k"] = store.KeyCounterSnapshot{Today: 4, Month: 7, TokensMonth: 9, LastReset: lastReset.UTC()}
		if err := mw.LoadFromStore(st); err != nil {
			t.Fatalf("LoadFromStore: %v", err)
		}
		today, month, _, _, _, _, _, _ = mw.KeyStats("k")
		return today, month
	}

	// Local midnight today is the previous day in UTC: today must be kept.
	if today, month := load(localMidnight); today != 4 || month != 7 {
		t.Errorf("reset at local midnight today: got (%d,%d), want (4,7)", today, month)
	}
	// Last reset late yesterday (local): today rolls over, month survives
	// unless yesterday was in the previous month.
	today, month := load(localMidnight.Add(-30 * time.Minute))
	if today != 0 {
		t.Errorf("reset yesterday: today = %d, want 0", today)
	}
	if now.Day() != 1 && month != 7 {
		t.Errorf("reset yesterday: month = %d, want 7", month)
	}
	// Local first of month 00:00 is the previous month in UTC: month must be kept.
	if _, month := load(monthStart); month != 7 {
		t.Errorf("reset at local month start: month = %d, want 7", month)
	}
	// Last reset in the previous local month: everything rolls over.
	if today, month := load(monthStart.Add(-time.Hour)); today != 0 || month != 0 {
		t.Errorf("reset last month: got (%d,%d), want (0,0)", today, month)
	}
}
