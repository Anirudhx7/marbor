package router

// prefixlocality.go - Prefix Locality Hints (Step 6, public ROADMAP.md).
//
// A weak, tier-5 placement-scoring signal: when a new request's prompt
// shares a prefix hash with a recent request, the node that handled the
// earlier one gets a small score bonus in computeNodeScore (placement.go),
// improving the odds the backend's KV cache still holds the shared prefix
// (faster time-to-first-token). This can NEVER override warm residency or
// any Hard Constraint - see placement.go's computeNodeScore for the weight
// ordering guarantee.
//
// Only a one-way hash of (model + prompt prefix) is ever stored, never raw
// prompt text - see PrefixHash.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// PrefixHash returns a one-way hash of model + the first n characters of the
// request's prompt/message text (see ExtractPromptPrefix), used as the key
// for prefix-locality routing hints. Including model in the hash input is
// required: a model's weights/warm-state are node-specific per model, so two
// different models sharing a similar prompt prefix must never collide onto
// the same routing hint.
func PrefixHash(model, promptPrefix string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + promptPrefix))
	return hex.EncodeToString(sum[:])
}

// ExtractPromptPrefix returns up to n characters of the request body's
// prompt/message text, for hashing into a prefix-locality routing hint.
// Returns "" for non-text or multimodal request bodies (no prompt/messages
// text field decodes) - hashing an empty string is harmless, it just means
// such requests collapse onto one low-value hash bucket.
func ExtractPromptPrefix(body []byte, n int) string {
	var req struct {
		Prompt   string `json:"prompt"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	text := req.Prompt
	if text == "" && len(req.Messages) > 0 {
		text = req.Messages[len(req.Messages)-1].Content
	}
	if len(text) > n {
		text = text[:n]
	}
	return text
}

// PrefixLocalityEnabled reports whether the prefix-locality scoring signal is
// active. Immutable after New() - see the Router.prefixLocalityEnabled field
// comment for why no lock is needed.
func (r *Router) PrefixLocalityEnabled() bool {
	return r.prefixLocalityEnabled
}

// lookupPrefixLocality returns the node name that last served prefixHash, or
// "" if unknown or prefixHash is empty.
func (r *Router) lookupPrefixLocality(prefixHash string) string {
	if prefixHash == "" {
		return ""
	}
	r.prefixMu.RLock()
	defer r.prefixMu.RUnlock()
	return r.prefixLocality[prefixHash]
}

// recordPrefixLocality updates the in-memory hint and, if a store is
// configured, persists it (write-through, matching RecordTransition's
// pattern for predictive_history). No-op if prefixHash is empty.
func (r *Router) recordPrefixLocality(prefixHash, nodeName string, now time.Time) {
	if prefixHash == "" {
		return
	}
	r.prefixMu.Lock()
	if r.prefixLocality == nil {
		r.prefixLocality = make(map[string]string)
	}
	r.prefixLocality[prefixHash] = nodeName
	r.prefixMu.Unlock()

	r.mu.RLock()
	st := r.store
	r.mu.RUnlock()
	if st != nil {
		_ = st.AppendPrefixLocality(prefixHash, nodeName, now)
	}
}

// SeedPrefixLocalityHistory replaces the in-memory prefix-locality map with
// persisted entries loaded at boot, applied in the same order they were
// recorded so the latest entry for a given hash wins - identical in spirit
// to SeedPredictiveHistory. Without this, every restart would silently reset
// all locality hints to zero (a correctness regression for long-running
// deployments, not just a minor perf loss).
func (r *Router) SeedPrefixLocalityHistory(entries []store.PrefixLocalityEntry) {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.PrefixHash] = e.NodeName
	}
	r.prefixMu.Lock()
	r.prefixLocality = m
	r.prefixMu.Unlock()
}

// PrefixLocalityStats reports cumulative hit/miss counts for the dashboard
// and admin API. A "hit" is a request whose prefix hash matched a recorded
// hint (regardless of whether that node was ultimately selected - warm
// residency and other higher-tier signals can still win); a "miss" is a
// request with no matching hint. Real counters only, never estimated (R1).
func (r *Router) PrefixLocalityStats() (hits, misses int64) {
	return r.prefixHitsTotal.Load(), r.prefixMissesTotal.Load()
}
