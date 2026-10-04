package bootstrapcred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerate_LengthAlphabetAndUniqueness(t *testing.T) {
	// Arrange
	seen := map[string]bool{}

	for i := 0; i < 50; i++ {
		// Act
		pw, err := Generate()

		// Assert
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(pw) < 24 {
			t.Fatalf("password length = %d, want at least 24", len(pw))
		}
		if pw == DefaultPassword {
			t.Fatal("generated the public default password")
		}
		for _, r := range pw {
			if !strings.ContainsRune(passwordAlphabet, r) {
				t.Fatalf("password contains %q outside the alphabet", r)
			}
		}
		if seen[pw] {
			t.Fatal("generated the same password twice")
		}
		seen[pw] = true
	}
	for _, ambiguous := range "0O1lI" {
		if strings.ContainsRune(passwordAlphabet, ambiguous) {
			t.Errorf("alphabet contains ambiguous glyph %q", ambiguous)
		}
	}
}

func TestMarker_RoundTripAndValidation(t *testing.T) {
	created := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, origin := range []Origin{OriginFresh, OriginUpgraded, OriginLegacyRecovery} {
		t.Run(string(origin), func(t *testing.T) {
			// Arrange
			in := Marker{Username: "admin", CreatedAt: created, Origin: origin}

			// Act
			raw, err := in.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			out, err := DecodeMarker(raw)

			// Assert
			if err != nil || out == nil {
				t.Fatalf("DecodeMarker: %v, %v", out, err)
			}
			if out.Username != in.Username || out.Origin != in.Origin || !out.CreatedAt.Equal(created) {
				t.Errorf("round trip = %+v, want %+v", *out, in)
			}
		})
	}
	if _, err := (Marker{Username: "x", Origin: "bogus"}).Encode(); err == nil {
		t.Error("Encode accepted an unknown origin")
	}
	if m, err := DecodeMarker("  "); m != nil || err != nil {
		t.Errorf("blank marker = %v, %v; want nil, nil", m, err)
	}
	for _, bad := range []string{"{", `{"username":"","origin":"fresh"}`, `{"username":"a","origin":"nope"}`} {
		if m, err := DecodeMarker(bad); m != nil || err == nil {
			t.Errorf("DecodeMarker(%q) = %v, %v; want an error", bad, m, err)
		}
	}
}

func TestGraceActive(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"dev", false},
		{"", false},
		{"v0.24.0", true},
		{"v0.25.0", true},
		{"0.25.9", true},
		{"v0.26.0", false},
		{"v0.30.1", false},
		{"v1.0.0", false},
		{"vX.Y.Z", false},
		{"v0", false},
		{"0.x.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := GraceActive(tt.version); got != tt.want {
				t.Errorf("GraceActive(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestEnvTrue_OnlyExactTrue(t *testing.T) {
	for value, want := range map[string]bool{"true": true, "TRUE": false, "True": false, "1": false, "yes": false, "": false, " true": false} {
		got := EnvTrue(func(string) string { return value }, "X")
		if got != want {
			t.Errorf("EnvTrue(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestResolveSecret(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	empty := filepath.Join(dir, "empty")
	def := filepath.Join(dir, "default")
	shortFile := filepath.Join(dir, "short")
	mustWrite(t, good, "from-the-file-1\n")
	mustWrite(t, shortFile, "tooshort\n")
	mustWrite(t, empty, "\n")
	mustWrite(t, def, "admin")

	tests := []struct {
		name    string
		env     map[string]string
		wantPW  string
		wantSrc SecretSource
		wantErr bool
	}{
		{"nothing", nil, "", SourceNone, false},
		{"plain env", map[string]string{EnvPassword: "from-env-value"}, "from-env-value", SourceEnv, false},
		{"file", map[string]string{EnvPasswordFile: good}, "from-the-file-1", SourceFile, false},
		{"file wins over plain", map[string]string{EnvPasswordFile: good, EnvPassword: "from-env-value"}, "from-the-file-1", SourceFile, false},
		{"missing file fails closed", map[string]string{EnvPasswordFile: filepath.Join(dir, "nope"), EnvPassword: "from-env-value"}, "", SourceFile, true},
		{"empty file", map[string]string{EnvPasswordFile: empty}, "", SourceFile, true},
		{"default in file", map[string]string{EnvPasswordFile: def}, "", SourceFile, true},
		{"default in env", map[string]string{EnvPassword: "admin"}, "", SourceEnv, true},
		{"too short in env", map[string]string{EnvPassword: "short-pw"}, "", SourceEnv, true},
		{"too short in file", map[string]string{EnvPasswordFile: shortFile}, "", SourceFile, true},
		{"exactly the minimum length", map[string]string{EnvPassword: "123456789012"}, "123456789012", SourceEnv, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw, src, err := ResolveSecret(func(k string) string { return tt.env[k] })
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if pw != tt.wantPW || src != tt.wantSrc {
				t.Errorf("got (%q, %v), want (%q, %v)", pw, src, tt.wantPW, tt.wantSrc)
			}
			if err != nil && tt.env[EnvPassword] != "" && strings.Contains(err.Error(), tt.env[EnvPassword]) && tt.env[EnvPassword] != "admin" {
				t.Error("error leaks the supplied password")
			}
		})
	}
}

func TestSecretFile_WriteVerifyReadRemove(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), FileName)

	// Act
	if err := WriteNew(path, "s3cret-value"); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	err := WriteNew(path, "second")
	got, readErr := ReadSafe(path)

	// Assert
	if err == nil {
		t.Error("WriteNew replaced an existing file; want an exclusive create")
	}
	if readErr != nil || got != "s3cret-value" {
		t.Errorf("ReadSafe = %q, %v", got, readErr)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ok, _ := Exists(path); ok {
		t.Error("file still exists after Remove")
	}
	if err := Remove(path); err != nil {
		t.Errorf("Remove of a missing file = %v, want nil", err)
	}
}

func TestVerifySafe_RejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("wrong mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix permission bits")
		}
		p := filepath.Join(dir, "loose")
		mustWrite(t, p, "x")
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := VerifySafe(p); err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("VerifySafe = %v, want an error naming the path", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(dir, "target")
		mustWrite(t, target, "x")
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("cannot create symlink here: %v", err)
		}
		if err := VerifySafe(link); err == nil {
			t.Error("VerifySafe accepted a symlink")
		}
		if err := WriteNew(link, "x"); err == nil {
			t.Error("WriteNew wrote through a symlink")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := VerifySafe(dir); err == nil {
			t.Error("VerifySafe accepted a directory")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if err := VerifySafe(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("VerifySafe(missing) = %v, want not-exist", err)
		}
	})
}

type fakeAddr struct{ ip net.IP }

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return a.ip.String() + ":8080" }

func TestBindIsLoopback_Matrix(t *testing.T) {
	prev := lookupIP
	t.Cleanup(func() { lookupIP = prev })
	lookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "localhost":
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, nil
		case "sneaky":
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.7")}, nil
		}
		return nil, errors.New("no such host")
	}
	tcp := func(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 8080} }

	tests := []struct {
		name       string
		bound      net.Addr
		configured string
		want       bool
	}{
		{"ipv4 loopback", tcp("127.0.0.1"), "127.0.0.1:8080", true},
		{"other 127/8", tcp("127.1.2.3"), "127.1.2.3:8080", true},
		{"ipv6 loopback", tcp("::1"), "[::1]:8080", true},
		{"localhost all loopback", tcp("127.0.0.1"), "localhost:8080", true},
		{"localhost resolving to a LAN address", tcp("127.0.0.1"), "sneaky:8080", false},
		{"unresolvable name", tcp("127.0.0.1"), "ghost:8080", false},
		{"ipv4 wildcard", tcp("0.0.0.0"), "0.0.0.0:8080", false},
		{"ipv6 wildcard", tcp("::"), "[::]:8080", false},
		{"empty host wildcard", tcp("::"), ":8080", false},
		{"LAN address", tcp("192.168.1.20"), "192.168.1.20:8080", false},
		{"non-tcp address", fakeAddr{}, "x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := BindIsLoopback(tt.bound, tt.configured)
			if got != tt.want {
				t.Errorf("BindIsLoopback = %v (%s), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("a refusal must carry a reason")
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
