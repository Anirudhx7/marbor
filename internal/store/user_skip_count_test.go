package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCreateUserPersistsSkipPasswordCount guards the first-login rule that an
// account can be created already at its skip cap in a single insert, so a
// crash can never leave a skippable default account behind.
func TestCreateUserPersistsSkipPasswordCount(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.CreateUser(User{
		Username:           "capped",
		Role:               "admin",
		Status:             "active",
		PasswordHash:       "x",
		MustChangePassword: true,
		SkipPasswordCount:  3,
		CreatedAt:          time.Now(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := st.GetUserByUsername("capped")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.SkipPasswordCount != 3 {
		t.Fatalf("SkipPasswordCount = %d, want 3", got.SkipPasswordCount)
	}
	if !got.MustChangePassword {
		t.Fatal("MustChangePassword = false, want true")
	}
}

// TestChangeUserPasswordOnlyWinsOnMatchingHash guards the compare-and-swap that
// keeps two concurrent password changes from both being accepted.
func TestChangeUserPasswordOnlyWinsOnMatchingHash(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cas.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	id, err := st.CreateUser(User{
		Username: "cas", Role: "admin", Status: "active", PasswordHash: "old",
		MustChangePassword: true, SkipPasswordCount: 2, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	ok, err := st.ChangeUserPassword(id, "stale", "first")
	if err != nil || ok {
		t.Fatalf("change with a stale hash: ok=%v err=%v, want false/nil", ok, err)
	}
	ok, err = st.ChangeUserPassword(id, "old", "first")
	if err != nil || !ok {
		t.Fatalf("change with the current hash: ok=%v err=%v, want true/nil", ok, err)
	}
	ok, err = st.ChangeUserPassword(id, "old", "second")
	if err != nil || ok {
		t.Fatalf("second change from the same old hash: ok=%v err=%v, want false/nil", ok, err)
	}

	got, err := st.GetUserByID(id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.PasswordHash != "first" || got.MustChangePassword || got.SkipPasswordCount != 0 {
		t.Fatalf("after change: hash=%q mustChange=%v skip=%d, want first/false/0", got.PasswordHash, got.MustChangePassword, got.SkipPasswordCount)
	}
}
