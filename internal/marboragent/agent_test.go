package marboragent

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnIfTokenFlagUsed(t *testing.T) {
	cases := []struct {
		name      string
		tokenFlag string
		wantWarn  bool
	}{
		{"token flag used", "secret123", true},
		{"token flag empty (MARBOR_AGENT_SECRET env var or --enroll/--mesh path)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnIfTokenFlagUsed(&buf, c.tokenFlag)
			gotWarn := buf.Len() > 0
			if gotWarn != c.wantWarn {
				t.Fatalf("warnIfTokenFlagUsed(%q): got output=%q (warn=%v), want warn=%v", c.tokenFlag, buf.String(), gotWarn, c.wantWarn)
			}
			if gotWarn && strings.Contains(buf.String(), c.tokenFlag) {
				t.Fatalf("warnIfTokenFlagUsed must not echo the token value itself, got %q", buf.String())
			}
		})
	}
}
