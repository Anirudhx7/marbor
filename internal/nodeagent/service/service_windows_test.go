//go:build windows

package service

import (
	"strings"
	"testing"
	"time"
)

func TestWindowsBinPath(t *testing.T) {
	cfg := Config{
		BinaryPath: `C:\Program Files\ollama-mesh\ollama-mesh.exe`,
		Port:       9200,
		Token:      "sekret",
	}
	got := windowsBinPath(cfg)
	want := `"C:\Program Files\ollama-mesh\ollama-mesh.exe" agent --port=9200`
	if got != want {
		t.Errorf("windowsBinPath() = %q, want %q", got, want)
	}
	if strings.Contains(got, "--token") {
		t.Errorf("windowsBinPath() must not embed --token (visible via sc qc / Task Manager), got %q", got)
	}
}

func TestWindowsBinPath_WithRefreshInterval(t *testing.T) {
	cfg := Config{
		BinaryPath:      `C:\Program Files\ollama-mesh\ollama-mesh.exe`,
		Port:            9200,
		Token:           "sekret",
		RefreshInterval: 30 * time.Second,
	}
	got := windowsBinPath(cfg)
	want := `"C:\Program Files\ollama-mesh\ollama-mesh.exe" agent --port=9200 --refresh-interval=30s`
	if got != want {
		t.Errorf("windowsBinPath() = %q, want %q", got, want)
	}
}

func TestWindowsBinPath_NoSpacesStillQuoted(t *testing.T) {
	cfg := Config{
		BinaryPath: `C:\ollama-mesh\ollama-mesh.exe`,
		Port:       8080,
		Token:      "abc",
	}
	got := windowsBinPath(cfg)
	want := `"C:\ollama-mesh\ollama-mesh.exe" agent --port=8080`
	if got != want {
		t.Errorf("windowsBinPath() = %q, want %q", got, want)
	}
}

func TestParseBinaryPathFromQC(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "quoted path with args",
			out: `[SC] QueryServiceConfig SUCCESS

SERVICE_NAME: ollama-mesh-agent
        TYPE               : 10  WIN32_OWN_PROCESS
        START_TYPE         : 2   AUTO_START
        ERROR_CONTROL      : 1   NORMAL
        BINARY_PATH_NAME   : "C:\Program Files\ollama-mesh\ollama-mesh.exe" agent --port=9200 --token=sekret
        LOAD_ORDER_GROUP   :
        TAG                : 0
        DISPLAY_NAME       : ollama-mesh Node Agent
`,
			want: `C:\Program Files\ollama-mesh\ollama-mesh.exe`,
		},
		{
			name: "unquoted path no spaces",
			out: `SERVICE_NAME: ollama-mesh-agent
        BINARY_PATH_NAME   : C:\ollama-mesh\ollama-mesh.exe agent --port=8080
`,
			want: `C:\ollama-mesh\ollama-mesh.exe`,
		},
		{
			name: "missing line",
			out:  "SERVICE_NAME: ollama-mesh-agent\n        TYPE : 10\n",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBinaryPathFromQC(tt.out)
			if got != tt.want {
				t.Errorf("parseBinaryPathFromQC() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseStateFromQuery(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "running",
			out: `SERVICE_NAME: ollama-mesh-agent
        TYPE               : 10  WIN32_OWN_PROCESS
        STATE              : 4  RUNNING
                                (STOPPABLE, NOT_PAUSABLE, ACCEPTS_SHUTDOWN)
        WIN32_EXIT_CODE    : 0  (0x0)
        SERVICE_EXIT_CODE  : 0  (0x0)
        CHECKPOINT         : 0x0
        WAIT_HINT          : 0x0
`,
			want: "running",
		},
		{
			name: "stopped",
			out: `SERVICE_NAME: ollama-mesh-agent
        STATE              : 1  STOPPED
`,
			want: "stopped",
		},
		{
			name: "start pending",
			out: `SERVICE_NAME: ollama-mesh-agent
        STATE              : 2  START_PENDING
`,
			want: "start_pending",
		},
		{
			name: "unparseable falls back to raw trimmed output",
			out:  "  some unexpected sc.exe output  ",
			want: "some unexpected sc.exe output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStateFromQuery(tt.out)
			if got != tt.want {
				t.Errorf("parseStateFromQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}
