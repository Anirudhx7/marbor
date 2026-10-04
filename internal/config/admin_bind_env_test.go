package config

import "testing"

func TestApplyAdminBindOverride(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantApplied bool
		wantBind    string
		wantErr     bool
	}{
		{"unset keeps stored bind", "", false, ":8080", false},
		{"blank keeps stored bind", "   ", false, ":8080", false},
		{"loopback override", "127.0.0.1:8080", true, "127.0.0.1:8080", false},
		{"ipv6 override", "[::1]:9000", true, "[::1]:9000", false},
		{"missing port", "127.0.0.1", false, ":8080", true},
		{"bad port", "127.0.0.1:99999", false, ":8080", true},
		{"non-numeric port", "127.0.0.1:http", false, ":8080", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Admin.BindAddress = ":8080"

			applied, err := ApplyAdminBindOverride(cfg, tt.raw)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if applied != tt.wantApplied || cfg.Admin.BindAddress != tt.wantBind {
				t.Errorf("applied=%v bind=%q, want applied=%v bind=%q", applied, cfg.Admin.BindAddress, tt.wantApplied, tt.wantBind)
			}
		})
	}
}
