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
		"http://192.168.1.50:11434",
		"http://0x7f.0.0.1:11434",  // loopback written in legacy hex form
		"http://0300.0250.1.50",    // 192.168.1.50 written in octal
		"http://node1.lan.:11434",  // FQDN with one trailing dot
		"http://1.2.3.4.5",         // five parts: not an IPv4 form
		"http://256.1.1.1",         // first part out of range: not an IP
		"http://256.0.0.1",         // first part out of range: not an IP
		"http://1.2.3.256",         // last part out of range: not an IP
		"http://169.254",           // two parts = 169.0.0.254, not link-local
		"http://08.1.1.1",          // 8 is not an octal digit; plain decimal 8.1.1.1
		"http://0x.1",              // empty hex part: not an IP
		"http://169.254.169.254..", // two trailing dots: not an IP
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

		// Dotted hex/octal/mixed and short (1-3 part) legacy IPv4 forms. Each
		// of these is accepted by C resolvers as 169.254.169.254 or another
		// link-local address.
		"http://0xA9.0xFE.0xA9.0xFE/",
		"http://0XA9.0XFE.0XA9.0XFE:11434",
		"http://0251.0376.0251.0376/",
		"http://00251.0376.0251.0376/",
		"http://169.0xfe.169.0376/",
		"http://0251.254.169.254/",
		"http://169.254.43518/",
		"http://169.254.0xa9fe/",
		"http://169.16689662/",
		"http://169.0xfea9fe/",
		"http://0xA9.0xFE.1.1:80", // a different link-local address

		// One trailing dot is accepted by glibc on a plain quad.
		"http://169.254.169.254./latest/meta-data/",
		"https://169.254.169.254.:8443",

		// Dotless forms that were already covered; kept as regression pins.
		"http://2852039166",
		"http://0xA9FEA9FE",
		// Octal reading of a dotless all-digit host. The decimal reading of
		// this string overflows 32 bits, so only the octal reading flags it:
		// do not simplify the dual decimal/octal check away.
		"http://025177524776",

		// Already rejected by net.ParseIP plus the link-local check; pinned so
		// a later change to the parse order cannot reopen them.
		"http://[::ffff:169.254.169.254]/",
		"http://[::ffff:a9fe:a9fe]/",
		"http://user@0xA9.0xFE.0xA9.0xFE/",

		// A single trailing dot on every legacy form is accepted here.
		"http://0251.0376.0251.0376./",
		"http://169.254.43518./",
		"http://2852039166./",
		"http://025177524776./",

		// Leading zero in a dotted form: inet_aton rejects the 9 as an octal
		// digit, but a decimal reading gives 169.254.169.254, so the host is
		// refused if either reading is link-local.
		"http://0169.254.169.254/",

		// Intentional over-blocking: glibc does not resolve a trailing dot on
		// the all-hex form, but accepting one trailing dot on every legacy
		// form is simpler and safer than per-form exceptions.
		"http://0xA9.0xFE.0xA9.0xFE./",
	}
	for _, u := range blocked {
		if err := ValidateNodeURL(u); err == nil {
			t.Errorf("ValidateNodeURL(%q) = nil, want error (must reject link-local/metadata/invalid)", u)
		}
	}
}

// TestParseLegacyIPv4 pins the inet_aton grammar: one to four dot-separated
// parts in decimal, 0x hex or leading-zero octal, every part but the last at
// most 255, the last part filling the remaining bytes, and at most one
// trailing dot. An empty want means the host is not a legacy IPv4 literal.
func TestParseLegacyIPv4(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		// one part
		{"2852039166", "169.254.169.254"},
		{"0xA9FEA9FE", "169.254.169.254"},
		{"0Xa9fea9fe", "169.254.169.254"},
		{"025177524776", "169.254.169.254"}, // octal
		{"0", "0.0.0.0"},
		{"4294967295", "255.255.255.255"},
		{"4294967296", ""},
		{"0x100000000", ""},
		// two parts: last part is 24 bits
		{"169.16689662", "169.254.169.254"},
		{"169.16777215", "169.255.255.255"},
		{"169.16777216", ""},
		{"169.254", "169.0.0.254"},
		// three parts: last part is 16 bits
		{"169.254.43518", "169.254.169.254"},
		{"169.254.0xa9fe", "169.254.169.254"},
		{"169.254.65535", "169.254.255.255"},
		{"169.254.65536", ""},
		// four parts: every part is 8 bits
		{"169.254.169.254", "169.254.169.254"},
		{"0xA9.0xFE.0xA9.0xFE", "169.254.169.254"},
		{"0XA9.0XFE.0XA9.0XFE", "169.254.169.254"},
		{"0251.0376.0251.0376", "169.254.169.254"},
		{"169.0xfe.169.0376", "169.254.169.254"},
		{"0xaB.0Xcd.0xEf.0x01", "171.205.239.1"},
		{"255.255.255.255", "255.255.255.255"},
		{"255.255.255.256", ""},
		{"256.1.1.1", ""},
		{"1.256.1.1", ""},
		// octal edge cases
		{"010.0.0.1", "8.0.0.1"},
		{"08.1.1.1", ""},
		{"09.1.1.1", ""},
		{"1.2.3.08", ""},
		{"08", ""},
		// one trailing dot is accepted, two are not
		{"169.254.169.254.", "169.254.169.254"},
		{"2852039166.", "169.254.169.254"},
		{"169.254.169.254..", ""},
		// not IPv4 literals
		{"", ""},
		{".", ""},
		{"..", ""},
		{".1.2.3", ""},
		{"1..2", ""},
		{"1.2.3.4.5", ""},
		{"0x", ""},
		{"0x.1", ""},
		{"1.0x", ""},
		{"0xg1", ""},
		{"gpu-node.lan", ""},
		{"node1", ""},
		{"1.2.3.x", ""},
		{"-1", ""},
		{"+1", ""},
		{"1_000", ""},
	}
	for _, c := range cases {
		got := ""
		if ip := parseLegacyIPv4(c.host); ip != nil {
			got = ip.String()
		}
		if got != c.want {
			t.Errorf("parseLegacyIPv4(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}
