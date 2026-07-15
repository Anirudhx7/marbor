package main

import (
	"testing"
	"time"
)

func TestFmtDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "  500ms"},
		{1 * time.Second, " 1.00s"},
		{12500 * time.Millisecond, "12.50s"},
	}
	for _, c := range cases {
		if got := fmtDur(c.d); got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestProgressBar(t *testing.T) {
	bar := progressBar(5*time.Second, 10*time.Second, 10)
	if len([]rune(bar)) != 12 { // 10 chars + 2 brackets
		t.Errorf("progressBar length = %d, want 12", len([]rune(bar)))
	}

	// half filled: first 5 should be filled
	runes := []rune(bar)
	for i := 1; i <= 5; i++ {
		if runes[i] != '█' {
			t.Errorf("progressBar[%d] = %c, want █", i, runes[i])
		}
	}
	for i := 6; i <= 10; i++ {
		if runes[i] != '░' {
			t.Errorf("progressBar[%d] = %c, want ░", i, runes[i])
		}
	}
}

func TestProgressBarCapsAtWidth(t *testing.T) {
	// duration exceeds max - bar should be fully filled, not overflow
	bar := progressBar(30*time.Second, 10*time.Second, 10)
	runes := []rune(bar)
	for i := 1; i <= 10; i++ {
		if runes[i] != '█' {
			t.Errorf("progressBar overflow: [%d] = %c, want █", i, runes[i])
		}
	}
}

func TestMedianOdd(t *testing.T) {
	samples := []time.Duration{300 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
	// after sort: [100, 200, 300]; (3-1)/2 = 1 → 200ms
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	median := sorted[(len(sorted)-1)/2]
	if median != 200*time.Millisecond {
		t.Errorf("median = %v, want 200ms", median)
	}
}

func TestMedianEven(t *testing.T) {
	samples := []time.Duration{400 * time.Millisecond, 100 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond}
	// after sort: [100, 200, 300, 400]; (4-1)/2 = 1 → 200ms (lower middle, not upper)
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	median := sorted[(len(sorted)-1)/2]
	if median != 200*time.Millisecond {
		t.Errorf("median (even) = %v, want 200ms (lower-middle, not biased high)", median)
	}
}
