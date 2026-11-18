package store

import (
	"path/filepath"
	"testing"
)

func TestCountAdminUsersCountsOnlyActiveAdmins(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "count-admins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mk := func(name, role, status string) int64 {
		t.Helper()
		id, err := st.CreateUser(User{Username: name, Role: role, Status: status, PasswordHash: "x"})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		return id
	}
	mk("active-admin", "admin", "active")
	mk("suspended-admin", "admin", "suspended")
	mk("pending-admin", "admin", "pending")
	mk("active-user", "user", "active")
	deleted := mk("deleted-admin", "admin", "active")
	if err := st.SoftDeleteUser(deleted, "test"); err != nil {
		t.Fatalf("SoftDeleteUser: %v", err)
	}

	n, err := st.CountAdminUsers()
	if err != nil {
		t.Fatalf("CountAdminUsers: %v", err)
	}
	if n != 1 {
		t.Errorf("CountAdminUsers = %d, want 1 (only the active, non-deleted admin)", n)
	}
}
