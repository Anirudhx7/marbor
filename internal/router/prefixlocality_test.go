package router

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/store"
)

// TestPrefixHashIncludesModel verifies two different models sharing an
// identical prompt prefix hash to different values - required so a node's
// per-model warm-state can never collide across models (spec §3/§7).
func TestPrefixHashIncludesModel(t *testing.T) {
	h1 := PrefixHash("model-a", "the quick brown fox")
	h2 := PrefixHash("model-b", "the quick brown fox")
	if h1 == h2 {
		t.Error("PrefixHash must differ across models for an identical prompt prefix")
	}
	if h1 == "" || h2 == "" {
		t.Error("PrefixHash must never return an empty string for non-empty input")
	}
}

// TestExtractPromptPrefixMultimodal verifies a body with no text prompt/
// messages field (e.g. an embeddings-only or multimodal request) yields "",
// per spec §6 edge case 2 - never a crash or a partial garbage hash input.
func TestExtractPromptPrefixMultimodal(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"no text field", `{"input": [1, 2, 3]}`},
		{"malformed json", `not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractPromptPrefix([]byte(c.body), 200)
			if got != "" {
				t.Errorf("ExtractPromptPrefix(%q) = %q, want \"\"", c.body, got)
			}
		})
	}

	prompt := ExtractPromptPrefix([]byte(`{"prompt":"hello world"}`), 200)
	if prompt != "hello world" {
		t.Errorf("ExtractPromptPrefix prompt field = %q, want \"hello world\"", prompt)
	}
	messages := ExtractPromptPrefix([]byte(`{"messages":[{"role":"user","content":"hi there"}]}`), 200)
	if messages != "hi there" {
		t.Errorf("ExtractPromptPrefix messages field = %q, want \"hi there\"", messages)
	}
}

// TestExtractPromptPrefixTruncates verifies the hashed prefix is bounded to n
// characters regardless of prompt size (spec §3 - bounds hashing cost).
func TestExtractPromptPrefixTruncates(t *testing.T) {
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'a'
	}
	body := []byte(`{"prompt":"` + string(long) + `"}`)
	got := ExtractPromptPrefix(body, 200)
	if len(got) != 200 {
		t.Errorf("len(ExtractPromptPrefix(...)) = %d, want 200", len(got))
	}
}

// TestSeedPrefixLocalityHistory verifies boot reseed replays entries in
// order so the latest entry for a given hash wins, matching
// SeedPredictiveHistory's contract for predictive_history (L3: same shape,
// same test).
func TestSeedPrefixLocalityHistory(t *testing.T) {
	r := New(config.RoutingConfig{PrefixLocalityEnabled: true, PrefixLocalityWeight: 10}, nil, nil)
	r.SeedPrefixLocalityHistory([]store.PrefixLocalityEntry{
		{PrefixHash: "h1", NodeName: "node-a"},
		{PrefixHash: "h1", NodeName: "node-b"}, // later entry for same hash wins
		{PrefixHash: "h2", NodeName: "node-c"},
	})
	if got := r.lookupPrefixLocality("h1"); got != "node-b" {
		t.Errorf("lookupPrefixLocality(h1) = %q, want \"node-b\"", got)
	}
	if got := r.lookupPrefixLocality("h2"); got != "node-c" {
		t.Errorf("lookupPrefixLocality(h2) = %q, want \"node-c\"", got)
	}
	if got := r.lookupPrefixLocality("unknown"); got != "" {
		t.Errorf("lookupPrefixLocality(unknown) = %q, want \"\"", got)
	}
	if got := r.lookupPrefixLocality(""); got != "" {
		t.Errorf("lookupPrefixLocality(\"\") = %q, want \"\"", got)
	}
}
