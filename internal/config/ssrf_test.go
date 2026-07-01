package config

import "testing"

// TestValidateNodeURL locks in the SSRF guard: link-local / cloud-metadata
// hosts are rejected, while loopback, RFC1918 private, and public hosts (the
// legitimate homelab / on-prem / test cases) are allowed.
func TestValidateNodeURL(t *testing.T) {
	allowed := []string{
		"http://localhost:11434",
		"http://127.0.0.1:11434",
		"https://gpu-node.lan:11434",
		"http://192.168.1.7:11435",
		"http://10.0.0.5:11434",
		"http://172.16.4.2:8000",
		"https://api.example.com",
	}
	for _, u := range allowed {
		if err := ValidateNodeURL(u); err != nil {
			t.Errorf("ValidateNodeURL(%q) = %v, want nil (must allow loopback/private/public)", u, err)
		}
	}

	blocked := []string{
		"http://169.254.169.254/latest/meta-data/", // AWS/GCP/Azure metadata
		"http://169.254.169.254",
		"http://169.254.10.20:11434", // link-local range
		"ftp://169.254.169.254",      // wrong scheme too
		"http://",                    // no host
		"not-a-url",                  // no scheme/host
		"tcp://10.0.0.1:11434",       // non-http scheme
	}
	for _, u := range blocked {
		if err := ValidateNodeURL(u); err == nil {
			t.Errorf("ValidateNodeURL(%q) = nil, want error (must reject link-local/metadata/invalid)", u)
		}
	}
}
