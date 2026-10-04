package bootstrapcred

import (
	"fmt"
	"net"
)

// SecretSource says where an operator-supplied password came from.
type SecretSource int

// Secret sources.
const (
	// SourceNone means the operator supplied nothing; a password is generated.
	SourceNone SecretSource = iota
	// SourceFile is MARBOR_ADMIN_PASSWORD_FILE.
	SourceFile
	// SourceEnv is MARBOR_ADMIN_PASSWORD.
	SourceEnv
)

// ResolveSecret returns the operator-supplied initial password, if any. The
// file variable wins over the plain one. A supplied value that cannot be used
// (unreadable file, empty value, the public default "admin", shorter than
// MinPasswordLength) is an error: the caller must fail closed rather than fall
// back to generating or to a default. Errors name the variable or path, never
// the value.
func ResolveSecret(getenv func(string) string) (string, SecretSource, error) {
	if path := getenv(EnvPasswordFile); path != "" {
		pw, err := readPasswordFile(path)
		if err != nil {
			return "", SourceFile, fmt.Errorf("%s: %w", EnvPasswordFile, err)
		}
		if err := CheckPasswordPolicy(pw); err != nil {
			return "", SourceFile, fmt.Errorf("%s: %w", EnvPasswordFile, err)
		}
		return pw, SourceFile, nil
	}
	if pw := getenv(EnvPassword); pw != "" {
		if err := CheckPasswordPolicy(pw); err != nil {
			return "", SourceEnv, fmt.Errorf("%s: %w", EnvPassword, err)
		}
		return pw, SourceEnv, nil
	}
	return "", SourceNone, nil
}

// lookupIP resolves a hostname for the loopback check. Tests replace it.
var lookupIP = net.LookupIP

// BindIsLoopback decides, from the address the listener is actually bound to,
// whether the admin listener is loopback-only. It never trusts the configured
// string alone: the bound address must be in 127.0.0.0/8 or ::1, and when the
// configured host is a name, every address that name resolves to must be
// loopback too. Wildcard binds (empty host, 0.0.0.0, ::) are not loopback.
// The second return value is a short reason for logs.
func BindIsLoopback(bound net.Addr, configured string) (bool, string) {
	tcp, ok := bound.(*net.TCPAddr)
	if !ok || tcp == nil || tcp.IP == nil {
		return false, "the bound address is not a TCP address"
	}
	if tcp.IP.IsUnspecified() {
		return false, fmt.Sprintf("bound to the wildcard address %s", tcp.IP)
	}
	if !tcp.IP.IsLoopback() {
		return false, fmt.Sprintf("bound to non-loopback address %s", tcp.IP)
	}
	host, _, err := net.SplitHostPort(configured)
	if err != nil || host == "" || net.ParseIP(trimBrackets(host)) != nil {
		return true, ""
	}
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return false, fmt.Sprintf("host %q could not be resolved", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false, fmt.Sprintf("host %q resolves to non-loopback address %s", host, ip)
		}
	}
	return true, ""
}

// ConfiguredBindIsLoopback decides from the configured host:port string alone
// (no listener yet) whether the bind is loopback-only. A wildcard or empty host
// is not loopback; a name counts only when every address it resolves to is
// loopback and it resolves at all.
func ConfiguredBindIsLoopback(configured string) bool {
	host, _, err := net.SplitHostPort(configured)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(trimBrackets(host)); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

func trimBrackets(h string) string {
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		return h[1 : len(h)-1]
	}
	return h
}
