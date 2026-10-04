package bootstrapcred

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarker_SuppliedRoundTripAndLegacyDefault(t *testing.T) {
	m := Marker{Username: "admin", Origin: OriginFresh, Supplied: true}
	raw, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodeMarker(raw)

	if err != nil || got == nil || !got.Supplied {
		t.Fatalf("round trip = %+v (err %v), want supplied", got, err)
	}
	old, err := DecodeMarker(`{"username":"admin","created_at":"2026-01-01T00:00:00Z","origin":"fresh"}`)
	if err != nil || old == nil || old.Supplied {
		t.Errorf("a marker written before the field existed must decode as generated: %+v (err %v)", old, err)
	}
}

func TestMarker_PasswordSource(t *testing.T) {
	tests := []struct {
		name string
		m    Marker
		want PasswordSource
	}{
		{"fresh generated", Marker{Origin: OriginFresh}, PasswordSourceFile},
		{"fresh supplied", Marker{Origin: OriginFresh, Supplied: true}, PasswordSourceSupplied},
		{"upgraded", Marker{Origin: OriginUpgraded}, PasswordSourceDefault},
		{"legacy recovery", Marker{Origin: OriginLegacyRecovery}, PasswordSourceDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.PasswordSource(); got != tt.want {
				t.Errorf("PasswordSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckPasswordPolicy_CountsCharactersNotBytes(t *testing.T) {
	if err := CheckPasswordPolicy(strings.Repeat("é", MinPasswordLength)); err != nil {
		t.Errorf("12 two-byte characters must be accepted: %v", err)
	}
	if err := CheckPasswordPolicy(strings.Repeat("é", MinPasswordLength-1)); err == nil {
		t.Error("11 two-byte characters (22 bytes) must be refused")
	}
}

func TestConfiguredBindIsLoopback(t *testing.T) {
	prev := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "localhost":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "mixed":
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.1")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	defer func() { lookupIP = prev }()
	tests := []struct {
		bind string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"localhost:8080", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"[::]:8080", false},
		{"192.0.2.10:8080", false},
		{"mixed:8080", false},
		{"unresolvable:8080", false},
		{"not-a-host-port", false},
	}
	for _, tt := range tests {
		t.Run(tt.bind, func(t *testing.T) {
			if got := ConfiguredBindIsLoopback(tt.bind); got != tt.want {
				t.Errorf("ConfiguredBindIsLoopback(%q) = %v, want %v", tt.bind, got, tt.want)
			}
		})
	}
}

func TestResolveSecret_RejectsAPasswordFileThatIsNotARegularFile(t *testing.T) {
	_, _, err := ResolveSecret(func(k string) string {
		if k == EnvPasswordFile {
			return t.TempDir() // a directory
		}
		return ""
	})

	if err == nil {
		t.Fatal("a directory must not be accepted as a password file")
	}
}

func TestResolveSecret_FollowsASymlinkToARegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("Supplied-Via-Link-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	pw, src, err := ResolveSecret(func(k string) string {
		if k == EnvPasswordFile {
			return link
		}
		return ""
	})

	if err != nil || src != SourceFile || pw != "Supplied-Via-Link-1" {
		t.Errorf("pw=%q src=%v err=%v: a secret behind a symlink (Docker secrets) must be read", pw, src, err)
	}
}

func TestParseICACLS(t *testing.T) {
	const path = `C:\data\initial-admin-password`
	tests := []struct {
		name          string
		out           string
		wantPrincipal []string
		wantInherited bool
		wantErr       bool
	}{
		{
			name:          "single owner entry",
			out:           path + " HOST\\ops:(F)\r\n\r\nSuccessfully processed 1 files; Failed processing 0 files\r\n",
			wantPrincipal: []string{`HOST\ops`},
		},
		{
			name:          "several entries, one inherited, a principal with spaces",
			out:           path + " NT AUTHORITY\\SYSTEM:(I)(F)\r\n                              BUILTIN\\Users:(I)(RX)\r\n\r\nSuccessfully processed 1 files\r\n",
			wantPrincipal: []string{`NT AUTHORITY\SYSTEM`, `BUILTIN\Users`},
			wantInherited: true,
		},
		{name: "no entries at all fails closed", out: "Successfully processed 0 files; Failed processing 1 files\r\n", wantErr: true},
		{name: "empty output fails closed", out: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, inherited, err := parseICACLS(tt.out, path)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if strings.Join(got, "|") != strings.Join(tt.wantPrincipal, "|") || inherited != tt.wantInherited {
				t.Errorf("principals = %v inherited = %v, want %v / %v", got, inherited, tt.wantPrincipal, tt.wantInherited)
			}
		})
	}
}

func TestAclAllowsOnly(t *testing.T) {
	const account, sid = `HOST\ops`, "S-1-5-21-1"
	tests := []struct {
		name       string
		principals []string
		inherited  bool
		wantOK     bool
	}{
		{"only the account", []string{`host\OPS`}, false, true},
		{"only the sid", []string{sid}, false, true},
		{"SYSTEM and Administrators are tolerated", []string{`HOST\ops`, `NT AUTHORITY\SYSTEM`, `BUILTIN\Administrators`, "S-1-5-32-544"}, false, true},
		{"another principal", []string{`HOST\ops`, "Everyone"}, false, false},
		{"inherited", []string{`HOST\ops`}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aclAllowsOnly(tt.principals, tt.inherited, account, sid) == ""
			if got != tt.wantOK {
				t.Errorf("ok = %v, want %v", got, tt.wantOK)
			}
		})
	}
}
