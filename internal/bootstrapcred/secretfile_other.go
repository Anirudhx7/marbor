//go:build !unix

package bootstrapcred

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
)

// openNoFollow has no equivalent flag here; symlinks are rejected with an
// explicit Lstat check plus a same-file check after opening.
const openNoFollow = 0

// restrictFile limits the freshly created (still empty) file to the current
// user with icacls: inheritance is removed and only the user's SID is granted.
// It fails closed: when icacls is unavailable, the current user cannot be
// determined, or icacls refuses, an error is returned and the caller removes
// the file instead of leaving a secret under an inherited ACL.
func restrictFile(_ *os.File, path string) error {
	icacls, err := exec.LookPath("icacls")
	if err != nil {
		return errors.New("icacls is not available, so the file cannot be restricted to the service account")
	}
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return errors.New("the current user's SID could not be determined, so the file cannot be restricted to the service account")
	}
	out, err := exec.Command(icacls, path, "/inheritance:r", "/grant:r", "*"+u.Uid+":F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls: %w (%s)", err, string(out))
	}
	return nil
}

// openNonBlock has no equivalent here; the regular-file check after opening
// still rejects anything that is not a plain file.
const openNonBlock = 0

// hasSingleLink cannot read a link count without a file handle and platform
// syscalls, so it reports false. The only caller is the zero-overwrite scrub in
// Remove, which then skips the overwrite and simply unlinks the name, the safe
// choice for a file that might be hard-linked to someone else's content.
func hasSingleLink(fs.FileInfo) bool { return false }

// checkOwnerAndMode re-verifies the file's ACL with icacls on every use, not only
// when the file is created: inheritance must be off and the only entry must be
// the current user. It is best effort in that it trusts icacls's listing, and it
// fails closed: when icacls is unavailable, the user cannot be determined, the
// listing cannot be parsed, or any other principal has access, the file is
// refused with a message that says how to fix it.
func checkOwnerAndMode(path string, _ fs.FileInfo) error {
	icacls, err := exec.LookPath("icacls")
	if err != nil {
		return fmt.Errorf("%s cannot be verified: icacls is not available", path)
	}
	u, err := user.Current()
	if err != nil || u.Uid == "" {
		return fmt.Errorf("%s cannot be verified: the current user could not be determined", path)
	}
	out, err := exec.Command(icacls, path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s cannot be verified: icacls: %w", path, err)
	}
	principals, inherited, err := parseICACLS(string(out), path)
	if err != nil {
		return err
	}
	if problem := aclAllowsOnly(principals, inherited, u.Username, u.Uid); problem != "" {
		return fmt.Errorf("%s is not private to the service account: %s. Fix it with: icacls %q /inheritance:r /grant:r \"*%s:F\"", path, problem, path, u.Uid)
	}
	return nil
}
