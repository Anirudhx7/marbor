package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// userConfigDir is a function var (not a direct os.UserConfigDir call) so
// tests can redirect it to a temp directory instead of the real OS config
// dir - the saved session file must never touch the real filesystem outside
// a real login/logout invocation.
var userConfigDir = os.UserConfigDir

// savedSession is the persisted CLI session, written by "login" and consumed
// as the lowest-priority credential source by authenticatedClient. It
// deliberately has no expiry field: the server (internal/store's session
// table) is the sole source of truth for validity, so an expired or revoked
// session is always discovered via a real 401 from the server, never a
// local clock comparison that could drift from server reality.
type savedSession struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// sessionFilePath returns the path to the saved session file, creating no
// directories or files itself.
func sessionFilePath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "marbor", "session"), nil
}

// saveSession persists s to the session file with 0600 permissions - never
// world/group readable, since it carries a live bearer token. os.WriteFile
// only applies the given mode when it creates the file - if a file already
// exists there with looser permissions (e.g. restored from a backup made
// before this project existed, or touched by another tool), a plain
// WriteFile would silently leave those looser permissions in place. The
// explicit Chmod makes 0600 an invariant on every login, not just the first.
func saveSession(s savedSession) error {
	path, err := sessionFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// loadSession reads the saved session file. A missing file is not an error -
// it means no one has ever logged in (or logout already ran) - callers get
// (nil, nil) for that case.
func loadSession() (*savedSession, error) {
	path, err := sessionFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s savedSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// deleteSession removes the saved session file. A file that doesn't exist is
// treated as success - "logout" is idempotent.
func deleteSession() error {
	path, err := sessionFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
