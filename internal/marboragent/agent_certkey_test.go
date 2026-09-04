package marboragent

import "testing"

// TestValidateCertKeyFlags is a regression test for B1 finding AGENT-01: the
// foreground --cert/--key flags used to silently fall through to plaintext
// HTTP (with only a log line) when exactly one was set, letting the bearer
// token traverse the network unencrypted on a partial-flag typo. Both set
// and both empty are the two intentional shapes; anything else must fail.
func TestValidateCertKeyFlags(t *testing.T) {
	tests := []struct {
		name    string
		cert    string
		key     string
		wantErr bool
	}{
		{"both empty - plaintext", "", "", false},
		{"both set - TLS", "/etc/marbor/cert.pem", "/etc/marbor/key.pem", false},
		{"cert only", "/etc/marbor/cert.pem", "", true},
		{"key only", "", "/etc/marbor/key.pem", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCertKeyFlags(tt.cert, tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCertKeyFlags(%q, %q) error = %v, wantErr %v", tt.cert, tt.key, err, tt.wantErr)
			}
		})
	}
}
