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

// TestIncrementSkipCountRespectsLimitAndPendingFlag guards the single-statement
// counter bump: it only counts while a change is pending and below the cap, and
// it never touches the password hash.
func TestIncrementSkipCountRespectsLimitAndPendingFlag(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "skip.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	id, err := st.CreateUser(User{
		Username: "skipper", Role: "admin", Status: "active", PasswordHash: "keep",
		MustChangePassword: true, SkipPasswordCount: 1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	for i, want := range []bool{true, false} {
		ok, err := st.IncrementSkipCount(id, 2)
		if err != nil || ok != want {
			t.Fatalf("call %d: ok=%v err=%v, want %v/nil", i, ok, err, want)
		}
	}
	got, _ := st.GetUserByID(id)
	if got.SkipPasswordCount != 2 || got.PasswordHash != "keep" {
		t.Fatalf("after bumps: skip=%d hash=%q, want 2/keep", got.SkipPasswordCount, got.PasswordHash)
	}

	if ok, err := st.ChangeUserPassword(id, "keep", "new"); err != nil || !ok {
		t.Fatalf("ChangeUserPassword: ok=%v err=%v", ok, err)
	}
	ok, err := st.IncrementSkipCount(id, 99)
	if err != nil || ok {
		t.Fatalf("bump after change: ok=%v err=%v, want false/nil", ok, err)
	}
	got, _ = st.GetUserByID(id)
	if got.PasswordHash != "new" || got.SkipPasswordCount != 0 {
		t.Fatalf("after change: hash=%q skip=%d, want new/0", got.PasswordHash, got.SkipPasswordCount)
	}
}
