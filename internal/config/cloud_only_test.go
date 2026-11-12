package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestCloudOnly_Validate(t *testing.T) {
	long := strings.Repeat("a", MaxModelAliasNameLen+1)
	tooMany := make([]string, MaxCloudOnlyModels+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("model-%d", i)
	}
	cases := []struct {
		name    string
		list    []string
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty", []string{}, false},
		{"exact and prefix", []string{"llama3", "qwen*", "org/repo:tag"}, false},
		{"max length ok", []string{strings.Repeat("a", MaxModelAliasNameLen)}, false},
		{"bare star", []string{"*"}, true},
		{"mid star", []string{"a*b"}, true},
		{"leading star", []string{"*abc"}, true},
		{"two stars", []string{"ab**"}, true},
		{"empty entry", []string{""}, true},
		{"blank entry", []string{"   "}, true},
		{"leading space", []string{" llama3"}, true},
		{"inner whitespace", []string{"llama 3"}, true},
		{"control char", []string{"llama\n3"}, true},
		{"duplicate", []string{"llama3", "llama3"}, true},
		{"duplicate different case", []string{"Llama3", "llama3"}, true},
		{"over length", []string{long}, true},
		{"over count", tooMany, true},
		{"exactly max count", tooMany[:MaxCloudOnlyModels], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCloudOnlyModels(tc.list)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCloudOnlyModels(%v) error = %v, wantErr %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestCloudOnly_ConfigValidate(t *testing.T) {
	c := &Config{}
	c.Routing.CloudOnlyModels = []string{"ok", "*"}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a bare star entry")
	}
	c.Routing.CloudOnlyModels = []string{"ok", "family-*"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid list: %v", err)
	}
	c.Routing.CloudOnlyModels = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected the empty default: %v", err)
	}
}

func TestCloudOnly_Sanitize(t *testing.T) {
	in := []string{"llama3", "*", "bad name", "LLAMA3", "qwen*", "a*b", ""}
	res := SanitizeCloudOnlyModels(in)
	got, dropped := res.Kept, res.Dropped
	want := []string{"llama3", "qwen*"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if len(dropped) != 5 {
		t.Fatalf("dropped %d reasons, want 5: %v", len(dropped), dropped)
	}
	if err := ValidateCloudOnlyModels(got); err != nil {
		t.Fatalf("sanitized list must validate: %v", err)
	}

	if res := SanitizeCloudOnlyModels(nil); res.Kept != nil || res.Dropped != nil {
		t.Fatalf("nil in: got %v", res)
	}

	many := make([]string, MaxCloudOnlyModels+25)
	for i := range many {
		many[i] = fmt.Sprintf("model-%d", i)
	}
	res = SanitizeCloudOnlyModels(many)
	got, dropped = res.Kept, res.Dropped
	if len(got) != MaxCloudOnlyModels {
		t.Fatalf("kept %d, want %d", len(got), MaxCloudOnlyModels)
	}
	if len(dropped) == 0 {
		t.Fatal("surplus entries must be reported")
	}
	if err := ValidateCloudOnlyModels(got); err != nil {
		t.Fatalf("capped list must validate: %v", err)
	}
}
