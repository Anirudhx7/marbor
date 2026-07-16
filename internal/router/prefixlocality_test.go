package router

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
)

func ctxWithKey(name string) context.Context {
	if name == "" {
		return context.Background()
	}
	return context.WithValue(context.Background(), auth.KeyNameContextKey, name)
}

func newPrefixTestRouter() *Router {
	return New(config.RoutingConfig{Strategy: "warm-first", PrefixLocalityEnabled: true, PrefixLocalityWeight: 10}, nil, nil)
}

// --- Case 1: two-turn rolling locality ---
func TestPrefixLocality_TwoTurnRolling(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")

	turn1 := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"U1"}]}`)
	key1, hint1 := r.PrefixLocalityLookup(ctx, "model-x", turn1)
	if hint1 != "" {
		t.Fatalf("turn 1 should have no hint yet, got %q", hint1)
	}
	r.RecordPrefixLocality(key1, "node-a", true)

	turn2 := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"U1"},{"role":"assistant","content":"A1"},{"role":"user","content":"U2"}]}`)
	key2, hint2 := r.PrefixLocalityLookup(ctx, "model-x", turn2)
	if hint2 != "node-a" {
		t.Errorf("turn 2 hint = %q, want \"node-a\" (probe should match turn 1's [S,U1] record)", hint2)
	}
	if key2 == key1 {
		t.Error("turn 2's own record key must differ from turn 1's (different full sequence)")
	}
}

// --- Case 2: three-turn rolling locality ---
func TestPrefixLocality_ThreeTurnRolling(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")

	turn2 := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"U1"},{"role":"assistant","content":"A1"},{"role":"user","content":"U2"}]}`)
	key2, _ := r.PrefixLocalityLookup(ctx, "model-x", turn2)
	r.RecordPrefixLocality(key2, "node-b", true)

	turn3 := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"U1"},{"role":"assistant","content":"A1"},{"role":"user","content":"U2"},{"role":"assistant","content":"A2"},{"role":"user","content":"U3"}]}`)
	_, hint3 := r.PrefixLocalityLookup(ctx, "model-x", turn3)
	if hint3 != "node-b" {
		t.Errorf("turn 3 hint = %q, want \"node-b\" (must match recorded full turn-2 sequence)", hint3)
	}
}

// --- Case 3: history sensitivity - different histories never collide ---
func TestPrefixLocality_HistorySensitivity(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")

	convoA := []byte(`{"messages":[{"role":"user","content":"apples"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`)
	keyA, _ := r.PrefixLocalityLookup(ctx, "model-x", convoA)
	r.RecordPrefixLocality(keyA, "node-a", true)

	convoB := []byte(`{"messages":[{"role":"user","content":"oranges"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`)
	_, hintB := r.PrefixLocalityLookup(ctx, "model-x", convoB)
	if hintB != "" {
		t.Errorf("different early history must not collide, got hint %q", hintB)
	}
}

// --- Case 4: system-prompt divergence - same tail, different system prompt ---
func TestPrefixLocality_SystemPromptDivergence(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")

	seq1 := []byte(`{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"U1"}]}`)
	key1, _ := r.PrefixLocalityLookup(ctx, "model-x", seq1)
	r.RecordPrefixLocality(key1, "node-a", true)

	seq2 := []byte(`{"messages":[{"role":"system","content":"You are terse"},{"role":"user","content":"U1"}]}`)
	key2, hint2 := r.PrefixLocalityLookup(ctx, "model-x", seq2)
	if hint2 != "" {
		t.Errorf("a different system prompt must yield a different key, got hint %q", hint2)
	}
	if key1 == key2 {
		t.Error("different system prompts must hash to different keys")
	}
}

// --- Case 5: exact-repeat legacy prompt precedence ---
func TestPrefixLocality_LegacyPromptPrecedence(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")

	// A body with BOTH prompt and messages: prompt must take precedence as
	// the whole sequence, ignoring messages entirely.
	body := []byte(`{"prompt":"legacy text","messages":[{"role":"user","content":"ignored"}]}`)
	key1, _ := r.PrefixLocalityLookup(ctx, "model-x", body)
	r.RecordPrefixLocality(key1, "node-a", true)

	// Exact repeat of the same legacy prompt must hit.
	_, hint := r.PrefixLocalityLookup(ctx, "model-x", body)
	if hint != "node-a" {
		t.Errorf("exact-repeat legacy prompt hint = %q, want \"node-a\"", hint)
	}

	// A messages-only body with the same tail text must NOT match the
	// legacy-prompt record (different sequence shape entirely).
	messagesOnly := []byte(`{"messages":[{"role":"user","content":"legacy text"}]}`)
	_, hint2 := r.PrefixLocalityLookup(ctx, "model-x", messagesOnly)
	if hint2 != "" {
		t.Errorf("messages-shaped body must not match a legacy-prompt record, got %q", hint2)
	}
}

// --- Case 6: empty / single-message / malformed inputs never panic ---
func TestCanonicalSequence_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"empty body", ``, false},
		{"empty object", `{}`, false},
		{"malformed json", `not json`, false},
		{"empty messages array", `{"messages":[]}`, false},
		{"single message", `{"messages":[{"role":"user","content":"hi"}]}`, true},
		{"legacy prompt only", `{"prompt":"hi"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seq, ok := canonicalSequence([]byte(c.body))
			if ok != c.ok {
				t.Errorf("canonicalSequence(%q) ok = %v, want %v (seq=%v)", c.body, ok, c.ok, seq)
			}
		})
	}

	// Also drive the full router-level lookup/record path with the same
	// inputs to prove no panic end-to-end, not just at the parsing layer.
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")
	for _, c := range cases {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("PrefixLocalityLookup panicked on %q: %v", c.body, rec)
				}
			}()
			key, hint := r.PrefixLocalityLookup(ctx, "model-x", []byte(c.body))
			r.RecordPrefixLocality(key, "node-a", true)
			_ = hint
		}()
	}
}

// --- Case 7: multimodal / non-text content is a safe miss, never a garbage key ---
func TestCanonicalSequence_MultimodalSafeMiss(t *testing.T) {
	cases := []string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, // array content
		`{"messages":[{"role":"user","content":{"nested":"object"}}]}`,           // object content
		`{"messages":[{"role":"user","content":null}]}`,                          // null content
		`{"messages":[{"role":"user"}]}`,                                         // missing content field
	}
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")
	for _, body := range cases {
		key, hint := r.PrefixLocalityLookup(ctx, "model-x", []byte(body))
		if key != "" || hint != "" {
			t.Errorf("multimodal/non-text body %q must be a safe miss (key=%q hint=%q)", body, key, hint)
		}
	}
}

// --- Case 8: principal isolation - identical conversations under two keys share nothing ---
func TestPrefixLocality_PrincipalIsolation(t *testing.T) {
	r := newPrefixTestRouter()
	body := []byte(`{"messages":[{"role":"user","content":"same conversation"}]}`)

	keyA, _ := r.PrefixLocalityLookup(ctxWithKey("key-A"), "model-x", body)
	r.RecordPrefixLocality(keyA, "node-a", true)

	keyB, hintB := r.PrefixLocalityLookup(ctxWithKey("key-B"), "model-x", body)
	if hintB != "" {
		t.Errorf("key-B must not see key-A's locality entry, got hint %q", hintB)
	}
	if keyA == keyB {
		t.Error("identical conversation under two different keys must hash to different keys")
	}
}

// --- Case 9: anonymous domain behavior ---
func TestPrefixLocality_AnonymousDomain(t *testing.T) {
	r := newPrefixTestRouter()
	body := []byte(`{"messages":[{"role":"user","content":"anon convo"}]}`)

	keyAnon1, _ := r.PrefixLocalityLookup(ctxWithKey(""), "model-x", body)
	r.RecordPrefixLocality(keyAnon1, "node-a", true)

	// A second anonymous request with the identical conversation shares the
	// same reserved anonymous domain and should hit.
	_, hint := r.PrefixLocalityLookup(context.Background(), "model-x", body)
	if hint != "node-a" {
		t.Errorf("two anonymous requests with the same conversation should share state, got hint %q", hint)
	}

	// An authenticated key must never see the anonymous domain's entry.
	_, hintAuth := r.PrefixLocalityLookup(ctxWithKey("real-key"), "model-x", body)
	if hintAuth != "" {
		t.Errorf("an authenticated key must not see the anonymous domain's entry, got %q", hintAuth)
	}
}

// --- Case 16: TTL expiry ---
func TestPrefixLocalityStore_TTLExpiry(t *testing.T) {
	s := newPrefixLocalityStore()
	fakeNow := time.Now()
	s.now = func() time.Time { return fakeNow }

	s.set("h1", "node-a")
	if _, ok := s.lookup("h1"); !ok {
		t.Fatal("expected a hit immediately after set")
	}

	fakeNow = fakeNow.Add(prefixLocalityTTL + time.Second)
	if _, ok := s.lookup("h1"); ok {
		t.Error("expected a miss after TTL has elapsed")
	}
}

// --- Case 16b: sweep removes expired entries proactively ---
func TestPrefixLocalityStore_SweepAtHalfTTL(t *testing.T) {
	s := newPrefixLocalityStore()
	fakeNow := time.Now()
	s.now = func() time.Time { return fakeNow }
	s.set("h1", "node-a")

	// A sweep at TTL/2 (the actual cadence the router's ticker calls this
	// at) must NOT remove an entry that is only half-expired.
	fakeNow = fakeNow.Add(prefixLocalityTTL/2 + time.Second)
	s.sweep()
	if s.len() != 1 {
		t.Errorf("a half-expired entry must survive a TTL/2 sweep, len=%d", s.len())
	}

	// A second TTL/2 sweep (now past the full TTL) must remove it.
	fakeNow = fakeNow.Add(prefixLocalityTTL/2 + time.Second)
	s.sweep()
	if s.len() != 0 {
		t.Errorf("expected the entry gone once its age exceeds the full TTL, len=%d", s.len())
	}

	// A fresh entry must survive a sweep that runs before its own TTL.
	s.set("h2", "node-b")
	s.sweep()
	if s.len() != 1 {
		t.Errorf("a live entry must survive a sweep, len=%d", s.len())
	}
}

// --- Case 17: bounded state / oldest-first eviction / last-writer-wins ---
func TestPrefixLocalityStore_BoundedEviction(t *testing.T) {
	s := newPrefixLocalityStore()
	base := time.Now()
	for i := 0; i < prefixLocalityMaxEntries; i++ {
		t := base.Add(time.Duration(i) * time.Millisecond)
		s.now = func() time.Time { return t }
		s.set("h"+strconv.Itoa(i), "node-"+strconv.Itoa(i))
	}
	if s.len() != prefixLocalityMaxEntries {
		t.Fatalf("expected exactly %d entries, got %d", prefixLocalityMaxEntries, s.len())
	}

	// One more insert must evict the oldest ("h0") and stay at the cap.
	s.now = func() time.Time { return base.Add(time.Hour) }
	s.set("overflow", "node-overflow")
	if s.len() != prefixLocalityMaxEntries {
		t.Errorf("expected the cap to hold after overflow, got %d", s.len())
	}
	if _, ok := s.lookup("h0"); ok {
		t.Error("oldest entry (h0) should have been evicted first")
	}
	if _, ok := s.lookup("overflow"); !ok {
		t.Error("the new entry must be present after eviction")
	}

	// Last-writer-wins: re-setting an existing key updates its node without
	// growing the map.
	before := s.len()
	s.set("h1", "node-REPLACED")
	if s.len() != before {
		t.Errorf("re-setting an existing key must not grow the map, before=%d after=%d", before, s.len())
	}
	if node, ok := s.lookup("h1"); !ok || node != "node-REPLACED" {
		t.Errorf("last-writer-wins: lookup(h1) = (%q, %v), want (\"node-REPLACED\", true)", node, ok)
	}
}

// --- Case 19: concurrency / race safety under mixed-principal load ---
func TestPrefixLocalityStore_ConcurrentAccess(t *testing.T) {
	s := newPrefixLocalityStore()
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := "principal-" + strconv.Itoa(g) + "-" + strconv.Itoa(i%10)
				s.set(key, "node-"+strconv.Itoa(i%3))
				s.lookup(key)
				if i%25 == 0 {
					s.sweep()
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestPrefixLocality_RecordRequiresSuccess covers cases 12/13/D2: no record
// call ever stores state unless success is explicitly true - this is the
// single completion-gate proxy.go relies on.
func TestPrefixLocality_RecordRequiresSuccess(t *testing.T) {
	r := newPrefixTestRouter()
	ctx := ctxWithKey("key-a")
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	key, _ := r.PrefixLocalityLookup(ctx, "model-x", body)

	r.RecordPrefixLocality(key, "node-a", false) // timeout/cancellation/5xx/abort simulation
	if _, hint := r.PrefixLocalityLookup(ctx, "model-x", body); hint != "" {
		t.Errorf("a failed/incomplete request must never record, got hint %q", hint)
	}

	r.RecordPrefixLocality(key, "node-a", true)
	if _, hint := r.PrefixLocalityLookup(ctx, "model-x", body); hint != "node-a" {
		t.Errorf("a successful completion must record, got hint %q", hint)
	}
}

// TestPrefixLocality_DisabledFeatureNeverRecordsOrLooksUp confirms the whole
// pipeline is inert when the feature is off (disabled-feature parity at the
// prefixlocality.go layer - placement_test.go covers the scoring layer).
func TestPrefixLocality_DisabledFeatureNeverRecordsOrLooksUp(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first"}, nil, nil) // PrefixLocalityEnabled defaults false
	ctx := ctxWithKey("key-a")
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)

	key, hint := r.PrefixLocalityLookup(ctx, "model-x", body)
	if key != "" || hint != "" {
		t.Errorf("disabled feature must return empty key/hint, got (%q, %q)", key, hint)
	}
	r.RecordPrefixLocality("some-key", "node-a", true)
	if r.prefixStore.len() != 0 {
		t.Errorf("disabled feature must never write to the store, len=%d", r.prefixStore.len())
	}
}
