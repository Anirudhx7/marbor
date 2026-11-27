package main

import "testing"

func TestVramForZeroSizeIsZero(t *testing.T) {
	if got := vramFor("codellama:13b", 0); got != 0 {
		t.Errorf("vramFor with size 0 = %d, want 0", got)
	}
	if got := vramFor("mistral:7b", 100); got != 100 {
		t.Errorf("fully loaded model vram = %d, want 100", got)
	}
}

func TestContextForVaries(t *testing.T) {
	if contextFor("unknown:1b") != defaultContextLength {
		t.Errorf("unknown model must use the default context")
	}
	if contextFor("qwen2.5:14b") == contextFor("mistral:7b") {
		t.Errorf("context lengths should vary across models")
	}
}
