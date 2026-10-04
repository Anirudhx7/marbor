// Package bootstrapcred holds the pieces of the first-boot administrator
// credential flow that do not depend on the admin server: generating the
// initial password, storing it in a private file, the persisted marker that
// records an initial/default password is still active, the loopback check
// for the admin listener, and the environment switches that steer all of it.
//
// Nothing in this package ever logs, prints or returns a password in an error
// message. Errors name file paths and causes only.
package bootstrapcred

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Environment variable names. The password is never accepted as a command-line
// argument.
const (
	// EnvPasswordFile points at a file holding the initial admin password
	// (the preferred way for managed deployments and Docker secrets).
	EnvPasswordFile = "MARBOR_ADMIN_PASSWORD_FILE"
	// EnvPassword carries the initial admin password directly. Accepted, but
	// weaker: it is visible to the process environment and container metadata.
	EnvPassword = "MARBOR_ADMIN_PASSWORD"
	// EnvAllowInsecure is the single escape hatch that lets the admin
	// listener run on a non-loopback bind while a default password is active.
	// Only the exact value "true" turns it on.
	EnvAllowInsecure = "MARBOR_ALLOW_INSECURE_DEFAULT_ADMIN"
	// EnvAdminBind forces the admin bind address for this process only. It
	// wins over the stored setting and is never written back to it.
	EnvAdminBind = "MARBOR_ADMIN_BIND_ADDRESS"
	// EnvDemoMode marks a demo deployment (exact value "true"). Demo stacks
	// ship a public admin login by design and are exempt from the refusal.
	EnvDemoMode = "MARBOR_DEMO_MODE"
)

// FileName is the generated-password file inside the data directory.
const FileName = "initial-admin-password"

// SettingKey is the settings-table key that holds the marker.
const SettingKey = "bootstrap_admin_pending"

// DefaultPassword is the public factory password that old installs still use.
const DefaultPassword = "admin"

// Origin says how the pending initial/default credential came to exist.
type Origin string

// Marker origins.
const (
	// OriginFresh is a credential generated (or operator-supplied) on a new
	// install. Enforced from the first boot.
	OriginFresh Origin = "fresh"
	// OriginUpgraded is a pre-existing factory admin/admin database found on
	// first boot of this version. It gets one release of warn-only grace.
	OriginUpgraded Origin = "upgraded"
	// OriginLegacyRecovery is an account recreated on the public default
	// password by the legacy credential recovery. Enforced immediately.
	OriginLegacyRecovery Origin = "legacy_recovery"
)

// Valid reports whether o is one of the known origins.
func (o Origin) Valid() bool {
	return o == OriginFresh || o == OriginUpgraded || o == OriginLegacyRecovery
}

// Marker is the persisted record that an initial/default password is active.
type Marker struct {
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	Origin    Origin    `json:"origin"`
	// Supplied is true when the operator chose the initial password (file or
	// environment) instead of the server generating one. A supplied credential
	// is never regenerated or overwritten. Markers written before this field
	// existed decode as false, which is the generated case.
	Supplied bool `json:"supplied,omitempty"`
}

// PasswordSource names where the pending password comes from, for the sign-in
// screen. It never carries a password.
type PasswordSource string

// Password sources reported to a loopback client.
const (
	// PasswordSourceFile is a generated password stored in the data directory.
	PasswordSourceFile PasswordSource = "file"
	// PasswordSourceDefault is the public default password.
	PasswordSourceDefault PasswordSource = "default"
	// PasswordSourceSupplied is a password the operator configured.
	PasswordSourceSupplied PasswordSource = "supplied"
)

// PasswordSource derives where the pending password comes from: a generated
// fresh credential lives in a file, an operator-supplied one is whatever the
// operator configured, and every other origin is the public default.
func (m Marker) PasswordSource() PasswordSource {
	switch {
	case m.Origin == OriginFresh && m.Supplied:
		return PasswordSourceSupplied
	case m.Origin == OriginFresh:
		return PasswordSourceFile
	}
	return PasswordSourceDefault
}

// Encode serializes the marker for the settings table.
func (m Marker) Encode() (string, error) {
	if m.Username == "" || !m.Origin.Valid() {
		return "", fmt.Errorf("bootstrapcred: invalid marker")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("bootstrapcred: encode marker: %w", err)
	}
	return string(b), nil
}

// DecodeMarker parses a stored marker. An empty string means no marker and
// returns (nil, nil); a malformed or incomplete value is an error.
func DecodeMarker(raw string) (*Marker, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var m Marker
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("bootstrapcred: decode marker: %w", err)
	}
	if m.Username == "" || !m.Origin.Valid() {
		return nil, fmt.Errorf("bootstrapcred: marker is incomplete")
	}
	return &m, nil
}

// passwordAlphabet leaves out glyphs that are easy to confuse when read off a
// terminal: 0/O, 1/l/I.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// PasswordLength is the length of a generated password (about 137 bits).
const PasswordLength = 24

// Generate returns a new random password from crypto/rand.
func Generate() (string, error) {
	max := big.NewInt(int64(len(passwordAlphabet)))
	out := make([]byte, PasswordLength)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("bootstrapcred: random source failed: %w", err)
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// graceThroughMinor is the last 0.x release whose upgraded databases are only
// warned about. The next release enforces the refusal. It is a build-time
// constant on purpose: grace never depends on wall-clock time.
const graceThroughMinor = 25

// GraceActive reports whether the one-release warn-only grace for upgraded
// default-password databases still applies to a build with this version string
// (for example "v0.25.1"). It fails closed: a version that cannot be parsed
// (a source or development build) has no grace, so those builds enforce the
// refusal immediately.
func GraceActive(version string) bool {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return false
	}
	if major > 0 {
		return false
	}
	return minor <= graceThroughMinor
}

// MinPasswordLength is the shortest password accepted for an operator-supplied
// initial secret and for a password change.
const MinPasswordLength = 12

// CheckPasswordPolicy rejects the public default password and anything shorter
// than MinPasswordLength characters (runes, not bytes, so the count matches what
// the operator sees). The error never contains the password.
func CheckPasswordPolicy(password string) error {
	if password == DefaultPassword {
		return fmt.Errorf("the public default password is not accepted")
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

// EnvTrue reports whether the named variable is exactly the string "true".
// Any other value, including "TRUE" and "1", is off.
func EnvTrue(getenv func(string) string, name string) bool {
	return getenv(name) == "true"
}
