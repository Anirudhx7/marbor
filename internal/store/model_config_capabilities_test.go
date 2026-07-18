package store

import "testing"

func contains(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// TestSupportedFieldsForMlx verifies mlx's field set matches mlx_lm.server's
// documented /v1/chat/completions request schema (SERVER.md): seed and
// response_format are excluded (not documented there), while top_k, min_p,
// repetition_penalty, and logit_bias are included (documented extras beyond
// strict OpenAI).
func TestSupportedFieldsForMlx(t *testing.T) {
	fields := SupportedFieldsFor("mlx")

	for _, unsupported := range []string{"seed", "response_format"} {
		if contains(fields, unsupported) {
			t.Errorf("SupportedFieldsFor(mlx) should not include %q: %v", unsupported, fields)
		}
	}
	for _, supported := range []string{"temperature", "top_p", "max_tokens", "stop", "top_k", "min_p", "repetition_penalty", "logit_bias"} {
		if !contains(fields, supported) {
			t.Errorf("SupportedFieldsFor(mlx) should include %q: %v", supported, fields)
		}
	}
}
