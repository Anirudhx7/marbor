package cli

import (
	"reflect"
	"testing"
)

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"both empty", "", "", 0},
		{"a empty", "", "abc", 3},
		{"b empty", "abc", "", 3},
		{"identical", "models", "models", 0},
		{"single insert", "cat", "cats", 1},
		{"single delete", "cats", "cat", 1},
		{"single substitute", "cat", "cot", 1},
		{"kitten sitting", "kitten", "sitting", 3},
		{"flaw lawn", "flaw", "lawn", 2},
		{"case sensitive differs", "Models", "models", 1},
		{"completely different", "abc", "xyz", 3},
		{"prefix relation", "mod", "models", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := levenshtein(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
			// Levenshtein distance is symmetric.
			gotRev := levenshtein(tt.b, tt.a)
			if gotRev != tt.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d (symmetry check)", tt.b, tt.a, gotRev, tt.want)
			}
		})
	}
}

func TestSuggest(t *testing.T) {
	commands := []string{"version", "status", "nodes", "models", "runtime"}

	tests := []struct {
		name       string
		input      string
		candidates []string
		want       []string
	}{
		{
			// "nodes" is also within edit distance 2 of "modles", so it is a
			// legitimate second suggestion; "models" must come first (it ties
			// on distance but wins the name-ascending tie-break).
			name:       "modles suggests models first",
			input:      "modles",
			candidates: commands,
			want:       []string{"models", "nodes"},
		},
		{
			name:       "runtim suggests runtime",
			input:      "runtim",
			candidates: commands,
			want:       []string{"runtime"},
		},
		{
			name:       "nothing close returns empty",
			input:      "xyzzy",
			candidates: commands,
			want:       []string{},
		},
		{
			name:       "prefix match returns both, closer first",
			input:      "mod",
			candidates: []string{"models", "modelconfig"},
			want:       []string{"models", "modelconfig"},
		},
		{
			// "models" itself must be excluded even though it is the input;
			// "nodes" is within edit distance 2 of "models" and is a
			// legitimate suggestion.
			name:       "exact match excluded",
			input:      "models",
			candidates: commands,
			want:       []string{"nodes"},
		},
		{
			name:       "empty candidates",
			input:      "models",
			candidates: []string{},
			want:       []string{},
		},
		{
			name:       "capped at three",
			input:      "nod",
			candidates: []string{"nodes", "node", "nodex", "nody", "nodz"},
			want:       []string{"node", "nody", "nodz"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suggest(tt.input, tt.candidates)
			// Treat nil and an empty slice as equal - suggest may return
			// either for "no matches" and callers should not care which.
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("suggest(%q, %v) = %v, want %v", tt.input, tt.candidates, got, tt.want)
			}
		})
	}
}
