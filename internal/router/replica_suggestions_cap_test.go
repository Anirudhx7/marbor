package router

import (
	"strings"
	"testing"
	"time"
)

func missingRankSuggestion(t *testing.T, nnodes int) ReplicaSuggestion {
	t.Helper()
	nodes := []nodeView{testRow("head", "host-a", 8000, "vllm")}
	hosts := evMap(
		hostEv("host-a", "a", []string{"10.0.0.1"}, vllmDep(8000, 0, nnodes, "10.0.0.1", 29500, false)),
	)
	got, _ := correlate(nodes, hosts)
	return findSuggestion(t, got, SuggestionIncomplete)
}

// The node count comes from the agent, so a huge value must not be expanded
// into one "missing rank" line per rank.
func TestCorrelate_VLLMHugeNodeCountBoundsMissingRanks(t *testing.T) {
	start := time.Now()
	s := missingRankSuggestion(t, 1<<20)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("correlation took %v for a huge node count", d)
	}
	if len(s.Missing) > 64 {
		t.Fatalf("missing list has %d entries, want it bounded", len(s.Missing))
	}
	if len(s.Reason) > 8192 {
		t.Fatalf("reason is %d bytes, want it bounded", len(s.Reason))
	}
	joined := strings.Join(s.Missing, "\n")
	if !strings.Contains(joined, "rank 1 has not been seen") || !strings.Contains(joined, "rank 32 has not been seen") {
		t.Errorf("first ranks should still be listed: %q", joined)
	}
	if strings.Contains(joined, "rank 33 has not been seen") {
		t.Errorf("ranks beyond the cap must be summarized: %q", joined)
	}
	if !strings.Contains(joined, "and 1048543 more ranks have not been seen") {
		t.Errorf("missing summary count: %q", joined)
	}
}

func TestCorrelate_VLLMSmallNodeCountListsEveryMissingRank(t *testing.T) {
	s := missingRankSuggestion(t, 4)
	joined := strings.Join(s.Missing, "\n")
	for _, r := range []string{"rank 1 has not been seen", "rank 2 has not been seen", "rank 3 has not been seen"} {
		if !strings.Contains(joined, r) {
			t.Errorf("missing %q in %q", r, joined)
		}
	}
	if strings.Contains(joined, " more ranks") {
		t.Errorf("small counts must not be summarized: %q", joined)
	}
}
