package nodeagent

// identity.go generates and persists this install's stable node_id
// (Agent.NodeID) - a UUID created once on first run and reused for the life
// of the install, surviving agent binary upgrades, hostname/IP/DNS changes,
// and runtime swaps. Stdlib-only (crypto/rand), no new dependency, matching
// every other zero-dependency choice in this package.

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// nodeIDFileName is the local state file the agent persists its generated
// node_id in.
const nodeIDFileName = "node_id"

// nodeIDDir returns the directory the agent persists its local state
// (currently just node_id) in, creating it if necessary. Falls back to the
// directory containing the running agent binary (not the process's current
// working directory) if the OS-standard per-user config dir can't be
// determined - a service/container invocation may not have $HOME/
// $XDG_CONFIG_HOME set, but its binary path is always the same regardless of
// what directory it was launched from, so this keeps node_id stable across
// restarts in exactly the headless environments most likely to hit this
// fallback. Only falls back to "." (truly last resort) if even that fails.
func nodeIDDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		if exe, exeErr := os.Executable(); exeErr == nil {
			base = filepath.Dir(exe)
		} else {
			base = "."
		}
	}
	dir := filepath.Join(base, "marbor-agent")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// loadOrCreateNodeID returns this install's stable node_id, generating and
// persisting a new one on first run. A read/parse failure (missing file,
// corrupt contents, permissions) falls back to generating (and trying to
// persist) a fresh one rather than failing agent startup - node_id is a
// fleet-identity/debugging aid, not something request handling depends on.
func loadOrCreateNodeID() string {
	path := filepath.Join(nodeIDDir(), nodeIDFileName)
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); isValidUUID(id) {
			return id
		}
	}
	id := newUUIDv4()
	_ = os.WriteFile(path, []byte(id), 0o600)
	return id
}

// newUUIDv4 generates a random (v4) UUID using only crypto/rand - the
// stdlib has no UUID helper, and pulling in a dependency for 20 lines of
// bit-twiddling would violate the project's zero-external-dependency rule.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively unrecoverable on any real OS;
		// a fixed placeholder is still a valid (if degenerate) stable
		// identifier rather than panicking agent startup over it.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// isValidUUID checks the canonical 8-4-4-4-12 hyphenated hex shape - just
// enough validation to reject a corrupt/truncated persisted file and
// regenerate, not a strict RFC 4122 version/variant check.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
