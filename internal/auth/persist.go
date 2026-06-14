package auth

// persist.go - durable per-key usage counters.
//
// Quotas and per-key usage are enforced/reported from in-memory counters.
// Without persistence a process restart (deploy, config reload, crash) would
// reset them, letting a key bypass its daily/monthly quota by forcing a
// restart and losing usage history. This snapshots the counters to a JSON file
// (atomic temp+rename) and restores them on startup.
//
// Durability model: the caller flushes periodically and on graceful shutdown,
// so a normal restart preserves counts. A hard crash loses at most one flush
// interval of counts - an honest, bounded window, documented as such. This
// keeps the single static binary (no CGO, no SQLite); a real datastore is the
// enterprise path on the roadmap.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
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

// SaveState writes per-key counters to path atomically. A blank path is a
// no-op (persistence disabled). Safe to call concurrently with request serving.
func (m *Middleware) SaveState(path string) error {
	if path == "" {
		return nil
	}
	m.mu.RLock()
	state := make(map[string]counterSnapshot, len(m.byName))
	for name, ks := range m.byName {
		state[name] = ks.counter.export()
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// Write + fsync the temp file before rename so a power loss cannot leave the
	// renamed file pointing at data that never reached disk (which would silently
	// reset quota counters - a quota-bypass vector). Rename is atomic vs torn
	// writes; fsync closes the crash-durability gap.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// fsync the parent directory so the rename itself is durable.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// LoadState restores counters from path. A missing file or blank path is a
// no-op (first run). Only counters for keys that still exist are restored;
// state for removed keys is ignored.
func (m *Middleware) LoadState(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var state map[string]counterSnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, snap := range state {
		if ks, ok := m.byName[name]; ok {
			ks.counter.restore(snap)
		}
	}
	return nil
}
