package auth

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
)

// TestKeyLookupIndexedByDigestNotRawToken is the regression test for
// b766b6c (fix(auth): make proxy key lookups constant-time via SHA-256
// digest). Before that fix, m.keys was indexed by the raw presented token
// (m.keys[k.Key]), so lookup timing depended on how many leading bytes of a
// guessed token matched a real key - a timing side channel an attacker
// could use to guess a valid key byte-by-byte. The fix keys the map by
// hashToken(token) instead, at every read/write site. This test asserts the
// raw token must never appear as a key in m.keys, and its SHA-256 digest
// must.
func TestKeyLookupIndexedByDigestNotRawToken(t *testing.T) {
	const rawToken = "sk-test-token-b766b6c"

	mw := NewMiddleware(config.AuthConfig{
		Enabled: config.BoolPtr(true),
		Keys: []config.KeyConfig{
			{Name: "test", Key: rawToken, RateLimit: 1000},
		},
	})

	mw.mu.RLock()
	_, rawIndexed := mw.keys[rawToken]
	_, digestIndexed := mw.keys[hashToken(rawToken)]
	mw.mu.RUnlock()

	if rawIndexed {
		t.Error("m.keys is indexed by the raw token - lookup timing again depends on how many leading bytes of a guessed token match (H6 timing side channel reintroduced)")
	}
	if !digestIndexed {
		t.Error("m.keys is not indexed by hashToken(token) - key created via NewMiddleware did not go through the digest keying path")
	}
}

// TestAddKeyAndRevokeKeyUseDigestIndexing extends the regression above to
// AddKey and RevokeKey, the other two m.keys read/write sites touched by
// b766b6c. Both must key and delete by digest, not by the raw token.
func TestAddKeyAndRevokeKeyUseDigestIndexing(t *testing.T) {
	const rawToken = "sk-test-token-addrevoke"

	mw := NewMiddleware(config.AuthConfig{Enabled: config.BoolPtr(true)})
	mw.AddKey(config.KeyConfig{Name: "added", Key: rawToken, RateLimit: 1000})

	mw.mu.RLock()
	_, rawIndexed := mw.keys[rawToken]
	_, digestIndexed := mw.keys[hashToken(rawToken)]
	mw.mu.RUnlock()
	if rawIndexed {
		t.Error("AddKey indexed m.keys by the raw token, want digest-only")
	}
	if !digestIndexed {
		t.Error("AddKey did not index m.keys by hashToken(token)")
	}

	mw.RevokeKey("added")
	mw.mu.RLock()
	_, stillPresent := mw.keys[hashToken(rawToken)]
	mw.mu.RUnlock()
	if stillPresent {
		t.Error("RevokeKey left the digest-indexed entry in m.keys - it deleted by the wrong key")
	}
}
