package cli

import "testing"

func TestFmtVRAMUsedAndTotal(t *testing.T) {
	cases := []struct {
		name      string
		n         NodeResp
		wantUsed  string
		wantTotal string
	}{
		{"declared only", NodeResp{VRAMTotalMB: 24000, VRAMSource: "declared"}, "-", "24000 MB (declared)"},
		{"no info", NodeResp{VRAMSource: "none"}, "-", "-"},
		{"old server no source no data", NodeResp{}, "-", "-"},
		{"agent reading", NodeResp{VRAMTotalMB: 81920, VRAMUsedMB: 1024, VRAMSource: "agent"}, "1024 MB", "81920 MB"},
		{"api reading no total", NodeResp{VRAMUsedMB: 512, VRAMSource: "api"}, "512 MB", "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmtVRAMUsed(tc.n); got != tc.wantUsed {
				t.Errorf("used = %q, want %q", got, tc.wantUsed)
			}
			if got := fmtVRAMTotal(tc.n); got != tc.wantTotal {
				t.Errorf("total = %q, want %q", got, tc.wantTotal)
			}
		})
	}
}
