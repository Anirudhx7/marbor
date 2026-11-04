package main

import (
	"path/filepath"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// A never-saved overflow SLA starts at the default, while a stored 0 stays 0
// (wait the full queue timeout) and any other stored value is kept.
func TestApplyPersistedSettings_OverflowSLADefault(t *testing.T) {
	cases := []struct {
		name   string
		stored *string
		want   int
	}{
		{"never saved", nil, defaultOverflowSLAMs},
		{"stored zero stays zero", strPtr("0"), 0},
		{"stored value kept", strPtr("500"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "marbor.db"))
			must(t, err)
			defer st.Close()
			if tc.stored != nil {
				must(t, st.SetSetting("routing_overflow_sla_ms", *tc.stored))
			}
			cfg := &config.Config{}
			applyPersistedSettings(cfg, st)
			if cfg.Routing.OverflowSLAMs != tc.want {
				t.Fatalf("OverflowSLAMs = %d, want %d", cfg.Routing.OverflowSLAMs, tc.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
