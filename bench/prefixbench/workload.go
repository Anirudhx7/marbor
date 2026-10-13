package main

// workload.go - deterministic synthetic traffic. The same (seed, repetition,
// workload) always produces the same message content, so every arm of a
// repetition sees traffic of identical shape. A short per-run tag at the head
// of each conversation keeps runs from reusing each other's cached prefixes
// (in the engine or in the router) while leaving everything after it identical.

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// convPlan is one client conversation: Turns[i] is the full message list sent
// on turn i+1 (history plus the new user message), the way a chat UI does.
type convPlan struct {
	ID    int
	Turns [][]chatMessage
}

var syllables = []string{"ka", "lo", "mi", "ren", "tu", "sha", "vor", "pel", "dun", "ix", "bra", "ome", "zil", "quo", "fen", "tarn"}

// makeWords returns n pseudo-words. They tokenize to a bit more than one token
// each in common tokenizers; the engine's reported counts are what matter.
func makeWords(rng *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		for s, k := 0, 1+rng.Intn(3); s < k; s++ {
			b.WriteString(syllables[rng.Intn(len(syllables))])
		}
	}
	return b.String()
}

// runSeed derives a stable seed for one (repetition, workload) pair.
func runSeed(base int64, rep int, workload string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%s", base, rep, workload)
	return int64(h.Sum64() >> 1)
}

// runTag is the per-run head tag. It differs for every run so no two runs
// share a prefix; it is the only content that does.
func runTag(runIndex int) string { return fmt.Sprintf("[ref run%d]", runIndex) }

// buildWorkload generates the conversations of one run.
func buildWorkload(spec workloadSpec, seed int64, tag string) []convPlan {
	rng := rand.New(rand.NewSource(seed))
	var system string
	if spec.SystemWords > 0 {
		system = tag + " You are a careful assistant. " + makeWords(rng, spec.SystemWords)
	}
	plans := make([]convPlan, spec.Conversations)
	for c := range plans {
		var history []chatMessage
		if system != "" {
			history = append(history, chatMessage{"system", system})
		}
		first := fmt.Sprintf("%s conversation %d: %s", tag, c, makeWords(rng, spec.ContextWords))
		if system != "" {
			first = fmt.Sprintf("conversation %d: %s", c, makeWords(rng, spec.ContextWords))
		}
		history = append(history, chatMessage{"user", first})
		for t := 0; t < spec.Turns; t++ {
			if t > 0 {
				history = append(history,
					chatMessage{"assistant", makeWords(rng, spec.AnswerWords)},
					chatMessage{"user", makeWords(rng, spec.TurnWords)})
			}
			plans[c].Turns = append(plans[c].Turns, append([]chatMessage(nil), history...))
		}
		plans[c].ID = c
	}
	return plans
}

// headKey is what the prefix-only arm hashes: the first message, serialized
// the way the router serializes it, cut to the router's 200-character bound.
// Conversations that share their first 200 characters therefore share a key.
func headKey(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	s := msgs[0].Role + "\x1f" + msgs[0].Content
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
