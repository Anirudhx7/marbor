package config

import (
	"strings"
	"testing"
)

func TestValidate_ModelAliases_RejectsSelfEmptyAndChained(t *testing.T) {
	cases := []struct {
		name    string
		aliases map[string]string
		wantErr string
	}{
		{"self", map[string]string{"gpt-4": "gpt-4"}, "itself"},
		{"empty alias", map[string]string{"": "llama3.2:8b"}, "alias must not be empty"},
		{"blank alias", map[string]string{"  ": "llama3.2:8b"}, "alias must not be empty"},
		{"empty target", map[string]string{"gpt-4": ""}, "target must not be empty"},
		{"untrimmed alias", map[string]string{" gpt-4": "llama3.2:8b"}, "whitespace"},
		{"untrimmed target", map[string]string{"gpt-4": "llama3.2:8b "}, "whitespace"},
		{"crlf alias", map[string]string{"gpt-4\r\nX-Evil: 1": "llama3.2:8b"}, "control characters"},
		{"newline target", map[string]string{"gpt-4": "llama\n3"}, "control characters"},
		{"too long", map[string]string{strings.Repeat("a", MaxModelAliasNameLen+1): "llama3.2:8b"}, "at most"},
		// Target is an alias: a -> b and b -> c.
		{"chained forward", map[string]string{"a": "b", "b": "c"}, "itself an alias"},
		// Alias name is another alias's target, declared the other way round.
		{"chained reverse", map[string]string{"z": "y", "x": "z"}, "itself an alias"},
		{"cycle", map[string]string{"a": "b", "b": "a"}, "itself an alias"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Routing.ModelAliases = tc.aliases
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_ModelAliases_AcceptsValidAndEmpty(t *testing.T) {
	for _, aliases := range []map[string]string{
		nil,
		{},
		{"gpt-4": "llama3.2:8b", "gpt-4o-mini": "qwen2.5:7b", "org/model:tag": "llama3.2:8b"},
	} {
		c := &Config{}
		c.Routing.ModelAliases = aliases
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(%v) = %v, want nil", aliases, err)
		}
	}
}

func TestSanitizeModelAliases_DropsInvalidKeepsValid(t *testing.T) {
	in := map[string]string{
		"gpt-4":       "llama3.2:8b",
		"self":        "self",
		"bad\nname":   "llama3.2:8b",
		"a":           "b",
		"b":           "c",
		"gpt-4o-mini": "qwen2.5:7b",
	}
	out, dropped := SanitizeModelAliases(in)
	want := map[string]string{"gpt-4": "llama3.2:8b", "gpt-4o-mini": "qwen2.5:7b", "b": "c"}
	if len(out) != len(want) {
		t.Fatalf("sanitized = %v, want %v", out, want)
	}
	for k, v := range want {
		if out[k] != v {
			t.Fatalf("sanitized[%q] = %q, want %q (full %v)", k, out[k], v, out)
		}
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped = %v, want 3 reasons", dropped)
	}
	if err := ValidateModelAliases(out); err != nil {
		t.Fatalf("sanitized map must validate, got %v", err)
	}
	// Input must not be mutated.
	if len(in) != 6 {
		t.Fatalf("input map mutated: %v", in)
	}
}

func TestSanitizeModelAliases_Empty(t *testing.T) {
	out, dropped := SanitizeModelAliases(nil)
	if out != nil || dropped != nil {
		t.Fatalf("SanitizeModelAliases(nil) = %v, %v; want nil, nil", out, dropped)
	}
}
