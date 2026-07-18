// Tests for the launchd Manager implementation. This machine has no real
// launchctl/root/darwin - so these only exercise the pure string-building
// (launchdPlistContent) and plist-parsing (extractBinaryPathFromPlist)
// functions, never actual launchctl exec calls. No build tag is needed:
// this file compiles and runs on any GOOS, same as service_darwin.go.
package service

import (
	"strings"
	"testing"
	"time"
)

func TestLaunchdPlistContent(t *testing.T) {
	cfg := Config{
		BinaryPath: "/usr/local/bin/ollama-mesh",
		Port:       9200,
		Token:      "sekret",
	}
	plist := launchdPlistContent(cfg)

	wantElements := []string{
		"<string>/usr/local/bin/ollama-mesh</string>",
		"<string>agent</string>",
		"<string>--port=9200</string>",
		"<key>Label</key>",
		"<string>com.ollamamesh.agent</string>",
		"<key>EnvironmentVariables</key>",
		"<key>TOKEN</key>",
		"<string>sekret</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
	}
	for _, want := range wantElements {
		if !strings.Contains(plist, want) {
			t.Errorf("launchdPlistContent missing %q in:\n%s", want, plist)
		}
	}

	// ProgramArguments must be a real argv array - each argument its own
	// XML element, not shell-joined into a single string.
	if strings.Contains(plist, "<string>agent --port=9200 --token=sekret</string>") {
		t.Errorf("ProgramArguments must not be a single shell-joined string")
	}
	// The token must never appear inside ProgramArguments (argv is visible
	// to any local user via ps/Activity Monitor) - only inside
	// EnvironmentVariables.
	if strings.Contains(plist, "--token") {
		t.Errorf("launchdPlistContent must not embed --token in ProgramArguments, got:\n%s", plist)
	}
}

func TestLaunchdPlistContent_RefreshInterval(t *testing.T) {
	cfg := Config{
		BinaryPath:      "/usr/local/bin/ollama-mesh",
		Port:            9200,
		Token:           "sekret",
		RefreshInterval: 30 * time.Second,
	}
	plist := launchdPlistContent(cfg)

	if !strings.Contains(plist, "<string>--refresh-interval=30s</string>") {
		t.Errorf("expected --refresh-interval element in:\n%s", plist)
	}
}

func TestLaunchdPlistContent_NoRefreshInterval(t *testing.T) {
	cfg := Config{
		BinaryPath: "/usr/local/bin/ollama-mesh",
		Port:       9200,
		Token:      "sekret",
	}
	plist := launchdPlistContent(cfg)

	if strings.Contains(plist, "--refresh-interval") {
		t.Errorf("did not expect --refresh-interval element when RefreshInterval is zero:\n%s", plist)
	}
}

func TestExtractBinaryPathFromPlist(t *testing.T) {
	cfg := Config{
		BinaryPath: "/usr/local/bin/ollama-mesh",
		Port:       9200,
		Token:      "sekret",
	}
	plist := launchdPlistContent(cfg)

	got, ok := extractBinaryPathFromPlist([]byte(plist))
	if !ok {
		t.Fatalf("extractBinaryPathFromPlist failed to extract from generated plist:\n%s", plist)
	}
	if got != cfg.BinaryPath {
		t.Errorf("extractBinaryPathFromPlist = %q, want %q", got, cfg.BinaryPath)
	}
}

func TestExtractBinaryPathFromPlist_NoProgramArguments(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.ollamamesh.agent</string>
</dict>
</plist>
`
	if _, ok := extractBinaryPathFromPlist([]byte(plist)); ok {
		t.Errorf("expected extraction to fail when ProgramArguments is absent")
	}
}

func TestExtractBinaryPathFromPlist_Malformed(t *testing.T) {
	if _, ok := extractBinaryPathFromPlist([]byte("not xml at all")); ok {
		t.Errorf("expected extraction to fail on malformed input")
	}
}

func TestParseLaunchctlListStatus(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "pid dict form",
			out:  "{\n\t\"PID\" = 1234;\n\t\"Label\" = \"com.ollamamesh.agent\";\n}\n",
			want: "running (pid 1234)",
		},
		{
			name: "column form running",
			out:  "1234\t0\tcom.ollamamesh.agent\n",
			want: "running (pid 1234)",
		},
		{
			name: "column form not running",
			out:  "-\t0\tcom.ollamamesh.agent\n",
			want: "loaded (not running)",
		},
		{
			name: "unrecognized format",
			out:  "some unexpected garbage\n",
			want: "loaded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseLaunchctlListStatus(tt.out); got != tt.want {
				t.Errorf("parseLaunchctlListStatus(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}
