package service

import (
	"strings"
	"testing"
	"time"
)

func TestSystemdUnitContent(t *testing.T) {
	cfg := Config{
		BinaryPath: "/usr/local/bin/ollama-mesh",
		Port:       9200,
		Token:      "sekret",
	}
	content := systemdUnitContent(cfg)

	wantExecStart := "ExecStart=/usr/local/bin/ollama-mesh agent --port=9200 --token=sekret"
	if !strings.Contains(content, wantExecStart) {
		t.Errorf("systemdUnitContent() missing expected ExecStart line %q, got:\n%s", wantExecStart, content)
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
		BinaryPath:      "/usr/local/bin/ollama-mesh",
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
		BinaryPath: "/opt/ollama-mesh/bin/ollama-mesh",
		Port:       9200,
		Token:      "sekret",
	}
	content := systemdUnitContent(cfg)

	got := execStartBinary(content)
	want := "/opt/ollama-mesh/bin/ollama-mesh"
	if got != want {
		t.Errorf("execStartBinary() = %q, want %q", got, want)
	}
}

func TestExecStartBinaryMissing(t *testing.T) {
	if got := execStartBinary("[Unit]\nDescription=nothing here\n"); got != "" {
		t.Errorf("execStartBinary() on content with no ExecStart = %q, want empty string", got)
	}
}
