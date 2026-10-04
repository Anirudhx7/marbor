package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ApplyAdminBindOverride replaces cfg.Admin.BindAddress with an address forced
// from the environment, which takes precedence over the stored setting. It is
// meant for the loopback recovery path: an install whose stored bind exposes
// the dashboard can be restarted on 127.0.0.1 without a logged-in session. The
// override is for this process only and is never written to the database.
//
// raw is the environment value; an empty (or blank) value means no override.
// A value that is not a host:port pair with a valid port is an error so a typo
// cannot silently leave the stored bind in force.
func ApplyAdminBindOverride(cfg *Config, raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	_, port, err := net.SplitHostPort(raw)
	if err != nil {
		return false, fmt.Errorf("admin bind override %q must be host:port: %w", raw, err)
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return false, fmt.Errorf("admin bind override %q has an invalid port", raw)
	}
	cfg.Admin.BindAddress = raw
	return true, nil
}
