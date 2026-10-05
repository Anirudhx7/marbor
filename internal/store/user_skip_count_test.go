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
