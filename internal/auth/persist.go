package auth

// persist.go - durable per-key usage counters.
//
// Quotas and per-key usage are enforced/reported from in-memory counters.
// SaveToStore/LoadFromStore flush counters to SQLite so a restart preserves
// quota state. A crash loses at most one flush interval of counts.

import (
	"time"

	storemod "github.com/Anirudhx7/marbor/internal/store"
)

// counterSnapshot is the persisted form of a keyCounter.
type counterSnapshot struct {
	Today       int       `json:"today"`
	Month       int       `json:"month"`
	TokensToday int64     `json:"tokensToday"`
	TokensMonth int64     `json:"tokensMonth"`
	LastReset   time.Time `json:"lastReset"`
}

func (c *keyCounter) export() counterSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.resetLocked(now)
	c.lastReset = now
	return counterSnapshot{
		Today:       c.today,
		Month:       c.month,
		TokensToday: c.tokensToday,
		TokensMonth: c.tokensMonth,
		LastReset:   c.lastReset,
	}
}

func (c *keyCounter) restore(s counterSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.today = s.Today
	c.month = s.Month
	c.tokensToday = s.TokensToday
	c.tokensMonth = s.TokensMonth
	c.lastReset = s.LastReset
}

// SaveToStore persists per-key usage counters to the SQLite store.
func (m *Middleware) SaveToStore(st storemod.Store) error {
	m.mu.RLock()
	snaps := make(map[string]counterSnapshot, len(m.byName))
	for name, ks := range m.byName {
		snaps[name] = ks.counter.export()
	}
	m.mu.RUnlock()

	for name, snap := range snaps {
		if err := st.SaveKeyCounters(name, storemod.KeyCounterSnapshot{
			Today:       snap.Today,
			Month:       snap.Month,
			TokensToday: snap.TokensToday,
			TokensMonth: snap.TokensMonth,
			LastReset:   snap.LastReset,
		}); err != nil {
			return err
		}
	}
	return nil
}

// LoadFromStore restores per-key usage counters from the SQLite store on startup.
func (m *Middleware) LoadFromStore(st storemod.Store) error {
	all, err := st.AllKeyCounters()
	if err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, snap := range all {
		if ks, ok := m.byName[name]; ok {
			ks.counter.restore(counterSnapshot{
				Today:       snap.Today,
				Month:       snap.Month,
				TokensToday: snap.TokensToday,
				TokensMonth: snap.TokensMonth,
				LastReset:   snap.LastReset,
			})
		}
	}
	return nil
}
