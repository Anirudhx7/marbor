package router

import "testing"

func TestFormatServable(t *testing.T) {
	runtimes := []string{"", "ollama", "llamacpp", "vllm", "tgi", "mlx", "sglang", "auto", "somethingnew"}
	cases := []struct {
		format string
		ok     map[string]bool // runtime -> servable, for the known formats
	}{
		{FormatOllamaLibrary, map[string]bool{"": true, "ollama": true}},
		{FormatGGUFHF, map[string]bool{"": true, "ollama": true, "llamacpp": true}},
	}
	for _, c := range cases {
		for _, rt := range runtimes {
			ok, known := FormatServable(rt, c.format)
			if !known {
				t.Errorf("FormatServable(%q, %q) known = false, want true", rt, c.format)
			}
			if ok != c.ok[rt] {
				t.Errorf("FormatServable(%q, %q) ok = %v, want %v", rt, c.format, ok, c.ok[rt])
			}
		}
	}
}

// Anything the matrix does not positively recognise must never exclude a node.
func TestFormatServable_UnknownNeverExcludes(t *testing.T) {
	formats := []string{FormatHFRepo, "fp8", "exl3", "safetensors", "", "auto", "not-a-format"}
	runtimes := []string{"", "ollama", "llamacpp", "vllm", "tgi", "mlx", "sglang", "auto"}
	for _, f := range formats {
		for _, rt := range runtimes {
			ok, known := FormatServable(rt, f)
			if known {
				t.Errorf("FormatServable(%q, %q) known = true, want false", rt, f)
			}
			if !ok {
				t.Errorf("FormatServable(%q, %q) ok = false: an unknown format must never exclude", rt, f)
			}
		}
	}
}
