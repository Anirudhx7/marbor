package router

// prefixlocality.go - rolling prefix-locality routing preference.
//
// A weak, additive placement-scoring signal (see the prefix_match term in
// placement.go's scoreComponents): when a new request's conversation history
// rolls forward from one that recently completed successfully, the node that
// completed the earlier one gets a small score bonus, improving the odds the
// backend's KV cache still holds the shared prefix (faster time-to-first-
// token). This can NEVER override warm residency, queue depth, or any Hard
// Constraint - see scoreComponents' doc comment for the weight ordering
// guarantee, and placement.go's routing-hierarchy doc comment for where this
// sits (weakest signal, optimization-tier only, never eligibility).
//
// Mechanism: the client-visible request history only. Generated assistant
// output is never captured, parsed, buffered, or inspected on this path - a
// lookup probes bounded suffix-truncations of the incoming conversation
// (canonicalSequence + prefixLookupCandidates), and a record stores only a
// one-way hash of the full served sequence plus the node name and a
// timestamp - never raw prompt/message content, key material, or a
// recoverable identity string.
//
// The in-memory prefixLocalityStore below is the SOLE runtime authority for
// lookups. SQLite (see seedPrefixLocalityFromStore and Router.RecordPrefixLocality's
// async write) is restart-continuity persistence only: it is never read on
// the routing hot path, and a persistence failure never fails, blocks, or
// degrades a request - losing durability just means a cold start on next
// restart, the same as if the feature had never run.
//
// Principal isolation: every hash is scoped by the calling API key's name
// (or a reserved anonymous identity for an unauthenticated request), so two
// different keys sending an identical conversation never share routing
// state. There is no tenant/organization/SSO concept here or anywhere else
// in the router - the API key is the only isolation boundary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/metrics"
)

const (
	// prefixLocalityMaxEntries/TTL are the locked state bounds - same
	// precedent as session affinity's maxAffinityEntries, applied to this
	// unrelated subsystem.
	prefixLocalityMaxEntries = 10_000
	prefixLocalityTTL        = 10 * time.Minute
	// prefixLocalityProbeDepth (K) bounds how many progressively-shorter
	// truncations of the incoming conversation a lookup will try before
	// giving up. Never unbounded - changing this requires explicit
	// justification, not casual tuning.
	prefixLocalityProbeDepth = 4
	// prefixLocalityMaxChars bounds the serialized candidate string fed into
	// the hash - applies ONLY to the candidate, never to domain or model.
	prefixLocalityMaxChars = 200
	// anonymousDomain is the reserved principal identity for an
	// unauthenticated request. Never equal to any real API key name (key
	// names are always non-empty, validated elsewhere), so an anonymous
	// request can never collide with, or be impersonated by, an
	// authenticated key's locality state.
	anonymousDomain = "\x00anonymous"
)

// rawChatBody is the minimal shape needed to extract a canonical keyed
// sequence from an already-buffered proxy request body, without depending on
// any runtime-specific request struct (the proxy's hot path parses the body
// ad hoc; there is no shared typed request struct to reuse). Content is
// decoded as json.RawMessage so a non-string content shape (an array of
// multimodal parts, for example) can be detected and safely rejected rather
// than partially serialized into a garbage key.
type rawChatBody struct {
	Prompt   string        `json:"prompt"`
	Messages []rawChatItem `json:"messages"`
}

type rawChatItem struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// canonicalSequence extracts the ordered list of "role\x1fcontent" strings
// this request's conversation resolves to. Returns ok=false for an
// empty/malformed body or any message whose content is not a plain JSON
// string (multimodal/non-text) - a safe miss, never a garbage key. Legacy
// non-empty `prompt` takes precedence over `messages` and is returned as a
// single-element sequence (whole-sequence semantics: it is never truncated
// for probing, only matched exactly - see prefixLookupCandidates).
func canonicalSequence(body []byte) (seq []string, ok bool) {
	var b rawChatBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, false
	}
	if b.Prompt != "" {
		return []string{"prompt\x1f" + b.Prompt}, true
	}
	if len(b.Messages) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(b.Messages))
	for _, m := range b.Messages {
		// Unmarshal into *string (not string) so a JSON `null` content value
		// is distinguishable from a genuine empty string "" - both an
		// unmarshal error (array/object content: multimodal) and a nil
		// result (null/absent content) are treated identically as a safe
		// miss for the whole request, never a partial/garbage key.
		var text *string
		if err := json.Unmarshal(m.Content, &text); err != nil || text == nil {
			return nil, false
		}
		out = append(out, m.Role+"\x1f"+*text)
	}
	return out, true
}

// serializeCandidate deterministically joins a (possibly truncated) message
// sequence into the string that gets hashed. Order-preserving; the
// delimiters are control characters that can never appear in a role name or
// ordinary request JSON, so they cannot be induced by client-supplied text
// to create a collision between two different sequences.
func serializeCandidate(seq []string) string {
	out := ""
	for i, s := range seq {
		if i > 0 {
			out += "\x1e"
		}
		out += s
	}
	return out
}

// prefixLookupCandidates returns up to K=4 progressively-shorter truncations
// of seq for LOOKUP only (record always hashes the full sequence via
// serializeCandidate(seq) directly, never through this function). For the
// legacy-prompt case (single-element seq) there is nothing to truncate, so
// no candidates are produced here - only an exact repeat of the same prompt
// can ever match, by design (a growing prompt string has no stable
// message-boundary to truncate at).
func prefixLookupCandidates(seq []string) []string {
	if len(seq) <= 1 {
		return nil
	}
	var out []string
	for i := 1; i <= prefixLocalityProbeDepth && len(seq)-i >= 1; i++ {
		out = append(out, serializeCandidate(seq[:len(seq)-i]))
	}
	return out
}

// prefixDomain resolves the isolation principal for ctx: the authenticated
// API key name, or the reserved anonymous domain for an unauthenticated
// request. This is the ONLY isolation boundary in the feature.
func prefixDomain(ctx context.Context) string {
	if name := auth.KeyNameFromContext(ctx); name != "" {
		// Namespaced so no operator-chosen key name - however unusual - can
		// ever equal anonymousDomain and share its keyspace.
		return "k\x00" + name
	}
	return anonymousDomain
}

// prefixHashKey computes sha256(domain + 0x00 + model + 0x00 + candidate[:200]).
// Truncation applies ONLY to candidate, never to domain or model.
func prefixHashKey(domain, model, candidate string) string {
	if len(candidate) > prefixLocalityMaxChars {
		candidate = candidate[:prefixLocalityMaxChars]
	}
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(candidate))
	return hex.EncodeToString(h.Sum(nil))
}

// prefixEntry is the only data ever retained per key - never raw content.
// seq is a monotonically increasing per-store write counter stamped at the
// time this entry was last set/refreshed - see prefixOrderSlot.
type prefixEntry struct {
	node string
	ts   time.Time
	seq  uint64
}

// prefixOrderSlot pairs a key with the seq it had at the moment it was
// appended to s.order. A refresh of an already-live key appends a NEW slot
// with a NEW seq rather than mutating or removing the old one (append-only,
// O(1) on the hot set() path) - so a key can have multiple slots in s.order
// at once. Only the slot whose seq still matches entries[key].seq is current;
// every older slot for the same key is a stale duplicate left behind by a
// refresh, and both evictOldestLocked and sweep must recognize and discard
// those duplicates rather than acting on them as if they were live.
type prefixOrderSlot struct {
	key string
	seq uint64
}

// prefixLocalityStore is a bounded, TTL-swept, last-writer-wins in-memory
// map: hash -> {node, timestamp}. This is the SOLE runtime authority for
// locality lookups - SQLite is restart-continuity persistence only.
// Independent mutex, held only for map operations, never across I/O or
// request lifecycle.
type prefixLocalityStore struct {
	mu      sync.Mutex
	entries map[string]prefixEntry
	order   []prefixOrderSlot // append order, oldest first - bounded at prefixLocalityMaxEntries; may contain stale duplicates for a refreshed key, see prefixOrderSlot
	nextSeq uint64
	now     func() time.Time
	hits    uint64
	misses  uint64
}

func newPrefixLocalityStore() *prefixLocalityStore {
	return &prefixLocalityStore{
		entries: make(map[string]prefixEntry),
		now:     time.Now,
	}
}

// lookup returns the recorded node for key if present and not expired.
// Counts a metrics hit/miss on every call (mirrors the CacheHit/CacheMiss
// convention used elsewhere in placement.go).
func (s *prefixLocalityStore) lookup(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || s.now().Sub(e.ts) > prefixLocalityTTL {
		s.misses++
		metrics.PrefixLocalityMiss()
		return "", false
	}
	s.hits++
	metrics.PrefixLocalityHit()
	return e.node, true
}

// set records/refreshes key -> node, last-writer-wins. Evicts the oldest
// still-current entry first if inserting a genuinely new key would exceed
// the cap. A refresh of an already-live key stamps a new seq and appends a
// new order slot rather than removing the old one - see prefixOrderSlot for
// why that is safe (evictOldestLocked/sweep discard the resulting stale
// duplicates by seq comparison instead of acting on them).
func (s *prefixLocalityStore) set(key, node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[key]; !exists && len(s.entries) >= prefixLocalityMaxEntries {
		s.evictOldestLocked()
	}
	s.nextSeq++
	seq := s.nextSeq
	s.entries[key] = prefixEntry{node: node, ts: s.now(), seq: seq}
	s.order = append(s.order, prefixOrderSlot{key: key, seq: seq})
}

// evictOldestLocked removes the single oldest entry still tracked in
// s.order, walking from the front and discarding (without touching
// s.entries) any slot whose seq no longer matches entries[key].seq - that
// slot is a stale duplicate left behind by a later refresh of the same key
// (see prefixOrderSlot), not a live oldest-entry candidate, so evicting the
// key it names here would wrongly discard a just-refreshed entry while a
// genuinely untouched key elsewhere in order survives. Only a slot whose seq
// still matches is current and gets evicted.
func (s *prefixLocalityStore) evictOldestLocked() {
	for len(s.order) > 0 {
		slot := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.entries[slot.key]; ok && e.seq == slot.seq {
			delete(s.entries, slot.key)
			return
		}
	}
}

// sweep removes every entry older than the TTL. Intended to be called
// periodically by Router's existing sweep ticker (see router.go's Start) -
// this method itself starts no goroutine and owns no lifecycle. Also drops
// any stale duplicate slot (seq mismatch, see prefixOrderSlot) from s.order
// without touching s.entries for it, so a refreshed key's order footprint
// shrinks back down to its single current slot instead of accumulating
// duplicates forever.
func (s *prefixLocalityStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	live := s.order[:0]
	for _, slot := range s.order {
		e, ok := s.entries[slot.key]
		if !ok || e.seq != slot.seq {
			continue // already removed, or a stale duplicate of a since-refreshed key
		}
		if now.Sub(e.ts) > prefixLocalityTTL {
			delete(s.entries, slot.key)
			continue
		}
		live = append(live, slot)
	}
	s.order = live
}

func (s *prefixLocalityStore) stats() (hits, misses uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.misses
}

// len reports the live entry count - test/reseed helper only.
func (s *prefixLocalityStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// PrefixLocalityEnabled reports whether the rolling-prefix-locality scoring
// signal is active. Immutable after New() - see the Router field comment.
func (r *Router) PrefixLocalityEnabled() bool {
	return r.prefixLocalityEnabled
}

// PrefixLocalityStats reports cumulative hit/miss counts for the dashboard
// and admin API. A "hit" is a request whose conversation matched a recorded
// entry (regardless of whether that node was ultimately selected - warm
// residency and other stronger signals can still win); a "miss" is a
// request with no matching entry. Real counters only, never estimated.
func (r *Router) PrefixLocalityStats() (hits, misses uint64) {
	return r.prefixStore.stats()
}

// PrefixLocalityLookup computes the record key for (ctx, model, body) and
// probes the in-memory store for a matching preferred node. Returns
// recordKey == "" when the feature is disabled or body does not resolve to a
// hashable conversation (multimodal, malformed, or empty) - callers should
// treat that as "nothing to look up or record for this request" and pass it
// straight through to RecordPrefixLocality, which no-ops on an empty key.
// Never touches SQLite - the in-memory store is the sole runtime authority.
func (r *Router) PrefixLocalityLookup(ctx context.Context, model string, body []byte) (recordKey, preferredNode string) {
	if !r.prefixLocalityEnabled {
		return "", ""
	}
	seq, ok := canonicalSequence(body)
	if !ok {
		return "", ""
	}
	domain := prefixDomain(ctx)
	full := serializeCandidate(seq)
	recordKey = prefixHashKey(domain, model, full)
	for _, cand := range prefixLookupCandidates(seq) {
		if node, hit := r.prefixStore.lookup(prefixHashKey(domain, model, cand)); hit {
			return recordKey, node
		}
	}
	// Also try the exact full-sequence key itself - covers an identical
	// exact-repeat request (e.g. the legacy-prompt case, which has no
	// truncated probe candidates at all) and any request whose length
	// didn't change turn-to-turn.
	if node, hit := r.prefixStore.lookup(recordKey); hit {
		return recordKey, node
	}
	return recordKey, ""
}

// RecordPrefixLocality stores/refreshes key -> nodeName in the in-memory
// store (sole runtime authority) on full successful completion only, and
// asynchronously best-effort persists it to SQLite for restart continuity.
// A no-op if disabled, success is false, key is empty (nothing hashable was
// found at lookup time), or nodeName is empty. Persistence failures are
// logged and never propagate - losing durability degrades to "reseed finds
// nothing on next restart", never a request-path failure. This is the ONLY
// place locality state is ever written - never at candidate selection,
// backend start, response headers, or first token.
func (r *Router) RecordPrefixLocality(key, nodeName string, success bool) {
	if !r.prefixLocalityEnabled || !success || key == "" || nodeName == "" {
		return
	}
	r.prefixStore.set(key, nodeName)
	r.mu.RLock()
	st := r.store
	r.mu.RUnlock()
	if st != nil {
		go func() {
			if err := st.AppendPrefixLocality(key, nodeName, time.Now()); err != nil {
				log.Printf("prefix-locality: persistence write failed (non-fatal, in-memory state unaffected): %v", err)
			}
		}()
	}
}

// seedPrefixLocalityFromStore reseeds the in-memory store from SQLite at
// boot (called from SetStore, once r.store is set). Entries older than the
// TTL, or whose node name is not among the currently-configured nodes, are
// dropped rather than resurrected - stale state from a since-removed or
// renamed node must never come back after a restart. Persistence errors
// here are logged and otherwise ignored: a reseed failure degrades to
// "starts with an empty map", never a boot failure - locality is a soft
// optimization, not a correctness dependency.
func (r *Router) seedPrefixLocalityFromStore() {
	if !r.prefixLocalityEnabled {
		return
	}
	st := r.warmStore()
	if st == nil {
		return
	}
	rows, err := st.PrefixLocalityHistory()
	if err != nil {
		log.Printf("prefix-locality: boot reseed read failed (starting with empty state): %v", err)
		return
	}
	r.mu.RLock()
	valid := make(map[string]bool, len(r.nodes))
	for _, n := range r.nodes {
		valid[n.Name] = true
	}
	r.mu.RUnlock()
	now := time.Now()
	seeded := 0
	for _, row := range rows {
		if now.Sub(row.Timestamp) > prefixLocalityTTL {
			continue // stale
		}
		if !valid[row.NodeName] {
			continue // node removed/renamed since this was recorded
		}
		r.prefixStore.set(row.PrefixHash, row.NodeName)
		seeded++
		if seeded >= prefixLocalityMaxEntries {
			break
		}
	}
}
