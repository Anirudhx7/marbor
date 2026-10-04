//go:build windows

package bootstrapcred

import (
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNew_WindowsACLIsLimitedToTheCurrentUser(t *testing.T) {
	icacls, err := exec.LookPath("icacls")
	if err != nil {
		t.Skip("icacls unavailable")
	}
	path := filepath.Join(t.TempDir(), FileName)

	if err := WriteNew(path, "secret-value-123"); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}

	out, err := exec.Command(icacls, path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v", err)
	}
	acl := string(out)
	for _, broad := range []string{"Everyone", "BUILTIN\\Users", "Authenticated Users"} {
		if strings.Contains(acl, broad) {
			t.Errorf("ACL still grants %q:\n%s", broad, acl)
		}
	}
	if u, err := user.Current(); err == nil && !strings.Contains(acl, u.Uid) && !strings.Contains(acl, u.Username) {
		t.Errorf("ACL does not name the current user:\n%s", acl)
	}
}

func TestReadSafe_WindowsReverifiesTheACLOnReuse(t *testing.T) {
	icacls, err := exec.LookPath("icacls")
	if err != nil {
		t.Skip("icacls unavailable")
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := WriteNew(path, "secret-value-123"); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if pw, err := ReadSafe(path); err != nil || pw != "secret-value-123" {
		t.Fatalf("a file this package just wrote must verify: pw=%q err=%v", pw, err)
	}

	if out, err := exec.Command(icacls, path, "/grant", "*S-1-1-0:R").CombinedOutput(); err != nil { // Everyone
		t.Skipf("could not widen the ACL: %v (%s)", err, out)
	}

	_, err = ReadSafe(path)

	if err == nil || !strings.Contains(err.Error(), "not private") {
		t.Errorf("a widened ACL must be refused with a clear error, got %v", err)
	}
}
