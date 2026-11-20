package cli

import "testing"

func TestIntDash(t *testing.T) {
	zero, n := int64(0), int64(1500)
	for _, tc := range []struct {
		in   *int64
		want string
	}{{nil, "-"}, {&zero, "0"}, {&n, "1500"}} {
		if got := intDash(tc.in); got != tc.want {
			t.Errorf("intDash(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
