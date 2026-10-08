package auth

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestKeyConfigFromRecordCarriesEveryField(t *testing.T) {
	rec := store.KeyRecord{
		Name: "k", Key: "sk-k", RateLimit: 5, DailyLimit: 6, MonthlyLimit: 7,
		DailyUsdCap: 1.25, MonthlyUsdCap: 9.5, Models: []string{"m1"},
		ExpiresAt: "2099-01-01", LocalOnly: true, AllowLocalDegradation: true,
	}
	cfg := KeyConfigFromRecord(rec)
	if cfg.DailyUsdCap != 1.25 || cfg.MonthlyUsdCap != 9.5 || !cfg.LocalOnly || !cfg.AllowLocalDegradation ||
		cfg.Name != "k" || cfg.Key != "sk-k" || cfg.RateLimit != 5 || cfg.DailyLimit != 6 ||
		cfg.MonthlyLimit != 7 || len(cfg.Models) != 1 || cfg.ExpiresAt != "2099-01-01" {
		t.Fatalf("config = %+v lost fields from record", cfg)
	}
	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	mw.AddKey(cfg)
	if d, m, ok := mw.KeyUsdCaps("k"); !ok || d != 1.25 || m != 9.5 {
		t.Errorf("caps = %v %v %v", d, m, ok)
	}
	if !mw.IsLocalOnly("k") || !mw.IsAllowLocalDegradation("k") {
		t.Error("local_only/allow_local_degradation not applied")
	}
}
