package cli

import "sort"

// This file adds a standalone "did you mean" suggestion helper, used by the
// dispatcher's unknown-token path.

// levenshtein computes the classic edit distance between a and b (insertions,
// deletions, substitutions each cost 1). It is case-sensitive; callers that
// want case-insensitive comparison should lowercase both inputs first. Uses a
// two-row rolling buffer so memory is O(min(len(a), len(b))).
func levenshtein(a, b string) int {
	// Ensure b is the shorter string so the row is as small as possible.
	if len(a) < len(b) {
		a, b = b, a
	}
	ra := []rune(a)
	rb := []rune(b)

	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)

	for j := 0; j <= len(rb); j++ {
		prev[j] = j
	}

	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			min := del
			if ins < min {
				min = ins
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}

	return prev[len(rb)]
}

// suggestCandidate pairs a candidate name with its computed distance from the
// input, for sorting.
type suggestCandidate struct {
	name string
	dist int
}

// suggest returns up to 3 candidate names from candidates that are plausible
// typo-corrections of input. A candidate is kept if:
//   - its Levenshtein distance from input is <= 2, or
//   - its Levenshtein distance from input is <= 3 and len(input) > 5, or
//   - it has input as a prefix, regardless of distance.
//
// Kept candidates are sorted by (distance ascending, name ascending) and
// capped at 3. A candidate exactly equal to input is always excluded.
func suggest(input string, candidates []string) []string {
	var kept []suggestCandidate
	for _, c := range candidates {
		if c == input {
			continue
		}
		d := levenshtein(input, c)
		isPrefix := len(c) >= len(input) && c[:len(input)] == input
		if d <= 2 || (d <= 3 && len(input) > 5) || isPrefix {
			kept = append(kept, suggestCandidate{name: c, dist: d})
		}
	}

	sort.Slice(kept, func(i, j int) bool {
		if kept[i].dist != kept[j].dist {
			return kept[i].dist < kept[j].dist
		}
		return kept[i].name < kept[j].name
	})

	if len(kept) > 3 {
		kept = kept[:3]
	}

	out := make([]string, len(kept))
	for i, k := range kept {
		out[i] = k.name
	}
	return out
}

// SuggestTopLevel returns up to 3 "did you mean" candidates for a mistyped
// top-level command word (os.Args[1] in main.go), using the same suggest()
// engine that already powers in-CLI typo correction (reportUnknownToken in
// dispatch.go) - so a typo of a top-level word and a typo of an in-CLI
// action get identical suggestion behavior.
//
// The candidate set is every name main.go's resolveCommand would actually
// route somewhere useful: TopLevelCommandNames() (every registered CLI
// command, INCLUDING Hidden ones like "completion" - a typo of a hidden
// command should still surface a correct suggestion, since Hidden only
// means "not advertised in --help", never "unreachable") unioned with the
// four other real top-level words resolveCommand recognizes that are not
// part of the internal/cli registry at all: "help", "bench", "agent",
// "uninstall". Dash-prefixed flag forms are deliberately never included -
// resolveCommand's dash check (main.go) runs before "unknown" is ever
// reached, so a dash-prefixed input never gets here to be suggested against.
func SuggestTopLevel(input string) []string {
	cliNames := TopLevelCommandNames()
	candidates := make([]string, 0, len(cliNames)+4)
	candidates = append(candidates, cliNames...)
	candidates = append(candidates, "help", "bench", "agent", "uninstall")
	return suggest(input, candidates)
}
