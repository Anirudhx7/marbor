package router

import "testing"

var (
	matrixRuntimes = []string{"", "ollama", "llamacpp", "vllm", "tgi", "mlx", "sglang", "auto", "somethingnew"}
	// Formats the matrix has no confident answer for.
	unknownFormats = []string{FormatHFRepo, "fp8", "exl3", "safetensors", "", "auto", "not-a-format"}
)

func TestFormatServable(t *testing.T) {
	// runtimes not listed for a format are expected NOT servable.
	cases := map[string]map[string]bool{
		FormatOllamaLibrary: {"": true, "ollama": true},
		FormatGGUFHF:        {"": true, "ollama": true, "llamacpp": true},
	}
	for format, servable := range cases {
		for _, rt := range matrixRuntimes {
			t.Run(format+"/"+rt, func(t *testing.T) {
				t.Parallel()
				ok, known := FormatServable(rt, format)
				if !known {
					t.Errorf("known = false, want true")
				}
				if ok != servable[rt] {
					t.Errorf("ok = %v, want %v", ok, servable[rt])
				}
			})
		}
	}
}

// Anything the matrix does not positively recognise must never exclude a node.
func TestFormatServable_UnknownNeverExcludes(t *testing.T) {
	for _, f := range unknownFormats {
		for _, rt := range matrixRuntimes {
			t.Run(f+"/"+rt, func(t *testing.T) {
				t.Parallel()
				ok, known := FormatServable(rt, f)
				if known {
					t.Errorf("known = true, want false")
				}
				if !ok {
					t.Errorf("ok = false: an unknown format must never exclude")
				}
			})
		}
	}
}

// The bucket names are shared with admin and appear in API-facing strings, so
// renaming a constant must be a deliberate, test-visible change.
func TestFormatConstants(t *testing.T) {
	if FormatOllamaLibrary != "ollama-library" || FormatGGUFHF != "gguf-hf" || FormatHFRepo != "hf-repo" {
		t.Errorf("format bucket names changed: %q %q %q", FormatOllamaLibrary, FormatGGUFHF, FormatHFRepo)
	}
}
