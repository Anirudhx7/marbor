package marboragent

import (
	"strings"
	"testing"
)

func TestIsLoopbackBind(t *testing.T) {
	tests := []struct {
		bind string
		want bool
	}{
		{"", false},
		{"0.0.0.0", false},
		{"::", false},
		{"10.0.0.5", false},
		{"example.com", false},
		{"localhost", false},
		{"127.0.0.1:9200", false},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"[::1]", true},
	}
	for _, tt := range tests {
		if got := isLoopbackBind(tt.bind); got != tt.want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", tt.bind, got, tt.want)
		}
	}
}

func TestValidatePlaintextBind(t *testing.T) {
	tests := []struct {
		name           string
		cert, key      string
		bind           string
		allowPlaintext bool
		wantErr        bool
	}{
		{"all interfaces plaintext refused", "", "", "", false, true},
		{"non-loopback plaintext refused", "", "", "10.0.0.5", false, true},
		{"loopback plaintext allowed", "", "", "127.0.0.1", false, false},
		{"ipv6 loopback plaintext allowed", "", "", "::1", false, false},
		{"explicit opt-in allows all interfaces", "", "", "", true, false},
		{"explicit opt-in allows non-loopback", "", "", "10.0.0.5", true, false},
		{"TLS allowed on all interfaces", "c.pem", "k.pem", "", false, false},
		{"TLS allowed on non-loopback", "c.pem", "k.pem", "10.0.0.5", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlaintextBind(tt.cert, tt.key, tt.bind, tt.allowPlaintext)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// The refusal must tell the operator how to fix it: serve TLS, bind to
// loopback, or opt in explicitly.
func TestValidatePlaintextBindErrorIsActionable(t *testing.T) {
	err := validatePlaintextBind("", "", "", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"--cert", "--key", "service install", "--bind=127.0.0.1", "--allow-insecure-plaintext"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
