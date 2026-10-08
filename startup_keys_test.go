package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestLoadStoredKeysRestoresCountersAndPolicy(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "marbor.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.UpsertKey(store.KeyRecord{
		Name: "team", Key: "sk-team", RateLimit: 100, DailyUsdCap: 2, MonthlyUsdCap: 30,
		LocalOnly: true, AllowLocalDegradation: true,
	}); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}
	if err := st.SaveKeyCounters("team", store.KeyCounterSnapshot{Today: 4, Month: 11, TokensMonth: 777, LastReset: time.Now()}); err != nil {
		t.Fatalf("SaveKeyCounters: %v", err)
	}

	mw := auth.NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	n, err := loadStoredKeys(mw, st)
	if err != nil {
		t.Fatalf("loadStoredKeys: %v", err)
	}
	if n != 1 {
		t.Fatalf("loaded %d keys, want 1", n)
	}
	today, month, tokens, _, _, _, _, ok := mw.KeyStats("team")
	if !ok || today != 4 || month != 11 || tokens != 777 {
		t.Errorf("counters = (%d,%d,%d,%v), want (4,11,777,true)", today, month, tokens, ok)
	}
	if d, m, _ := mw.KeyUsdCaps("team"); d != 2 || m != 30 {
		t.Errorf("caps = %v %v, want 2 30", d, m)
	}
	if !mw.IsLocalOnly("team") || !mw.IsAllowLocalDegradation("team") {
		t.Error("local_only / allow_local_degradation lost at startup")
	}
}
