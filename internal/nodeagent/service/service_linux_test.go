package service

import (
	"strings"
	"testing"
	"time"
)

func TestSystemdUnitContent(t *testing.T) {
	cfg := Config{
		BinaryPath: "/usr/local/bin/marbor",
		Port:       9200,
		Token:      "sekret",
	}
	content := systemdUnitContent(cfg)

	wantExecStart := "ExecStart=/usr/local/bin/marbor --port=9200"
	if !strings.Contains(content, wantExecStart) {
		t.Errorf("systemdUnitContent() missing expected ExecStart line %q, got:\n%s", wantExecStart, content)
	}
	if strings.Contains(content, "--token") {
		t.Errorf("systemdUnitContent() must not embed --token in ExecStart, got:\n%s", content)
	}
	if !strings.Contains(content, "EnvironmentFile="+tokenEnvFilePath) {
		t.Errorf("systemdUnitContent() missing EnvironmentFile=%s, got:\n%s", tokenEnvFilePath, content)
	}
	if !strings.Contains(content, "Restart=on-failure") {
		t.Errorf("systemdUnitContent() missing Restart=on-failure, got:\n%s", content)
	}
	if !strings.Contains(content, "WantedBy=multi-user.target") {
		t.Errorf("systemdUnitContent() missing WantedBy=multi-user.target, got:\n%s", content)
	}
	if !strings.Contains(content, "Type=simple") {
		t.Errorf("systemdUnitContent() missing Type=simple, got:\n%s", content)
	}
}

func TestSystemdUnitContentWithRefreshInterval(t *testing.T) {
	cfg := Config{
		BinaryPath:      "/usr/local/bin/marbor",
		Port:            9200,
		Token:           "sekret",
		RefreshInterval: 30 * time.Second,
	}
	content := systemdUnitContent(cfg)

	if !strings.Contains(content, "--refresh-interval=30s") {
		t.Errorf("systemdUnitContent() missing --refresh-interval=30s, got:\n%s", content)
	}
}

func TestExecStartBinary(t *testing.T) {
	cfg := Config{
		BinaryPath: "/opt/marbor/bin/marbor",
		Port:       9200,
		Token:      "sekret",
	}
	content := systemdUnitContent(cfg)

	got := execStartBinary(content)
	want := "/opt/marbor/bin/marbor"
	if got != want {
		t.Errorf("execStartBinary() = %q, want %q", got, want)
	}
}

func TestExecStartBinaryMissing(t *testing.T) {
	if got := execStartBinary("[Unit]\nDescription=nothing here\n"); got != "" {
		t.Errorf("execStartBinary() on content with no ExecStart = %q, want empty string", got)
	}
}

// TestSystemdUnitContentQuotesPathWithSpaces is a regression test: a
// BinaryPath containing a space (a supported install.sh INSTALL_DIR
// override) must be quoted in ExecStart= so systemd doesn't split it into
// two tokens, and execStartBinary must recover the full original path from
// that quoted form, not just the substring up to the first space.
func TestSystemdUnitContentQuotesPathWithSpaces(t *testing.T) {
	cfg := Config{
		BinaryPath: "/opt/my company/bin/marbor",
		Port:       9200,
		Token:      "sekret",
	}
	content := systemdUnitContent(cfg)

	wantExecStart := `ExecStart="/opt/my company/bin/marbor" --port=9200`
	if !strings.Contains(content, wantExecStart) {
		t.Errorf("systemdUnitContent() missing quoted ExecStart line %q, got:\n%s", wantExecStart, content)
	}

	got := execStartBinary(content)
	want := "/opt/my company/bin/marbor"
	if got != want {
		t.Errorf("execStartBinary() = %q, want %q (full path recovered from quotes)", got, want)
	}
}

// TestSystemdUnitContentNoQuotingWhenNotNeeded verifies the common case
// (no whitespace anywhere) is completely unaffected by the quoting fix -
// output is byte-identical to before.
func TestSystemdUnitContentNoQuotingWhenNotNeeded(t *testing.T) {
	cfg := Config{BinaryPath: "/usr/local/bin/marbor", Port: 9200, Token: "sekret"}
	content := systemdUnitContent(cfg)
	if strings.Contains(content, `"`) {
		t.Errorf("systemdUnitContent() should not add quotes when nothing needs them, got:\n%s", content)
	}
}
