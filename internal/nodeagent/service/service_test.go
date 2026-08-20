package service

import (
	"strings"
	"testing"
)

// TestConfigArgs_IncludesCertAndKeyOnlyWhenBothSet verifies Config.args()
// (shared by all three platform implementations' service-definition
// builders) appends --cert/--key only when both are set, and omits them
// entirely for a plaintext config - the "both empty means run plaintext"
// convention CertPath/KeyPath's doc comment describes.
func TestConfigArgs_IncludesCertAndKeyOnlyWhenBothSet(t *testing.T) {
	plain := Config{Port: 9200}
	got := strings.Join(plain.args(), " ")
	if strings.Contains(got, "--cert") || strings.Contains(got, "--key") {
		t.Errorf("plaintext Config.args() = %q, must not include --cert/--key", got)
	}

	tls := Config{Port: 9200, CertPath: "/etc/marbor-agent.crt", KeyPath: "/etc/marbor-agent.key"}
	got = strings.Join(tls.args(), " ")
	if !strings.Contains(got, "--cert=/etc/marbor-agent.crt") {
		t.Errorf("Config.args() = %q, want --cert=/etc/marbor-agent.crt", got)
	}
	if !strings.Contains(got, "--key=/etc/marbor-agent.key") {
		t.Errorf("Config.args() = %q, want --key=/etc/marbor-agent.key", got)
	}

	// A partial config (only one of CertPath/KeyPath set) is not a state any
	// caller in this codebase produces, but args() must still fail safe -
	// never emit a lone --cert or --key that would make the agent try (and
	// fail) to serve HTTPS with only half a pair.
	partial := Config{Port: 9200, CertPath: "/etc/marbor-agent.crt"}
	got = strings.Join(partial.args(), " ")
	if strings.Contains(got, "--cert") || strings.Contains(got, "--key") {
		t.Errorf("partial Config.args() = %q, must omit both --cert and --key when only one is set", got)
	}
}
