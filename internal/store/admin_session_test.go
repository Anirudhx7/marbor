package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The admin-credentials/session persistence layer (GetAdminCreds, SetAdminCreds,
// CreateSession, ValidateSession, DeleteSession, PruneExpiredSessions) backs the
// exact-match session-token guard but had zero test coverage (audit, 2026-09-05).
// hashSessionToken means a session is only ever readable/matchable via its
// digest - these tests confirm the round trip and the expiry/deletion boundaries
// actually work at the storage layer, not just in auth.go's in-memory checks.

func newTestStoreForSessions(t *testing.T) Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAdminCreds_RoundTrip(t *testing.T) {
	st := newTestStoreForSessions(t)

	if _, err := st.GetAdminCreds(); err != ErrNoAdminCreds {
		t.Fatalf("GetAdminCreds on empty store: got err %v, want ErrNoAdminCreds", err)
	}

	want := AdminCreds{Username: "admin", PasswordHash: "hash123", Salt: "salt456"}
	if err := st.SetAdminCreds(want); err != nil {
		t.Fatalf("SetAdminCreds: %v", err)
	}

	got, err := st.GetAdminCreds()
	if err != nil {
		t.Fatalf("GetAdminCreds: %v", err)
	}
	if got != want {
		t.Fatalf("GetAdminCreds = %+v, want %+v", got, want)
	}

	// SetAdminCreds must overwrite the single row (INSERT OR REPLACE), not
	// accumulate rows - a second admin_credentials row would make GetAdminCreds
	// non-deterministic.
	updated := AdminCreds{Username: "admin2", PasswordHash: "hash789", Salt: "saltabc"}
	if err := st.SetAdminCreds(updated); err != nil {
		t.Fatalf("SetAdminCreds (overwrite): %v", err)
	}
	got, err = st.GetAdminCreds()
	if err != nil {
		t.Fatalf("GetAdminCreds after overwrite: %v", err)
	}
	if got != updated {
		t.Fatalf("GetAdminCreds after overwrite = %+v, want %+v", got, updated)
	}
}

func TestSession_ValidateRejectsUnknownToken(t *testing.T) {
	st := newTestStoreForSessions(t)

	valid, err := st.ValidateSession("token-that-was-never-created")
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if valid {
		t.Fatal("ValidateSession must reject a token that was never created")
	}
}

func TestSession_CreateThenValidateThenDelete(t *testing.T) {
	st := newTestStoreForSessions(t)

	token := "a-real-random-session-token"
	if err := st.CreateSession(token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	valid, err := st.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if !valid {
		t.Fatal("ValidateSession must accept a freshly created, unexpired session")
	}

	// A near-miss token (differs by one character) must never validate - this
	// is the storage-layer half of the exact-match guard enforced in auth.go.
	nearMiss := "a-real-random-session-tokeX"
	valid, err = st.ValidateSession(nearMiss)
	if err != nil {
		t.Fatalf("ValidateSession(near-miss): %v", err)
	}
	if valid {
		t.Fatal("ValidateSession must not accept a token differing by one character")
	}

	if err := st.DeleteSession(token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	valid, err = st.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession after delete: %v", err)
	}
	if valid {
		t.Fatal("ValidateSession must reject a token after DeleteSession")
	}
}

func TestSession_ExpiredIsRejectedAndPruned(t *testing.T) {
	st := newTestStoreForSessions(t)

	token := "already-expired-token"
	if err := st.CreateSession(token, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	valid, err := st.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if valid {
		t.Fatal("ValidateSession must reject an expired session")
	}

	if err := st.PruneExpiredSessions(); err != nil {
		t.Fatalf("PruneExpiredSessions: %v", err)
	}

	// After pruning, the row is gone entirely (not just expired) - re-creating
	// the same token with a future expiry must succeed and validate.
	if err := st.CreateSession(token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession after prune: %v", err)
	}
	valid, err = st.ValidateSession(token)
	if err != nil {
		t.Fatalf("ValidateSession after re-create: %v", err)
	}
	if !valid {
		t.Fatal("ValidateSession must accept the re-created session post-prune")
	}
}
