package auth

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

func TestReloadAppliesUsdCapsToNewRotatedAndExistingKeys(t *testing.T) {
	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys:    []config.KeyConfig{{Name: "rot", Key: "sk-old", RateLimit: 10}, {Name: "same", Key: "sk-same", RateLimit: 10}},
	})
	mw.Reload(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "rot", Key: "sk-new", RateLimit: 10, DailyUsdCap: 1.5, MonthlyUsdCap: 20},
			{Name: "same", Key: "sk-same", RateLimit: 10, DailyUsdCap: 3, MonthlyUsdCap: 30},
			{Name: "fresh", Key: "sk-fresh", RateLimit: 10, DailyUsdCap: 2.5, MonthlyUsdCap: 40},
		},
	})
	want := map[string][2]float64{"rot": {1.5, 20}, "same": {3, 30}, "fresh": {2.5, 40}}
	for name, w := range want {
		d, mo, ok := mw.KeyUsdCaps(name)
		if !ok || d != w[0] || mo != w[1] {
			t.Errorf("%s caps = (%v, %v, %v), want (%v, %v, true)", name, d, mo, ok, w[0], w[1])
		}
	}
}
