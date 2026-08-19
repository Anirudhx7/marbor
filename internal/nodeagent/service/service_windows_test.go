//go:build windows

package service

import (
	"io"
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
	want := `"C:\Program Files\ollama-mesh\ollama-mesh.exe" --port=9200`
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
	want := `"C:\Program Files\ollama-mesh\ollama-mesh.exe" --port=9200 --refresh-interval=30s`
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
	want := `"C:\ollama-mesh\ollama-mesh.exe" --port=8080`
	if got != want {
		t.Errorf("windowsBinPath() = %q, want %q", got, want)
	}
}

func TestSetServiceTokenEnvCommand_ArgsNeverContainToken(t *testing.T) {
	const token = "sekret-node-agent-token"
	cmd := setServiceTokenEnvCommand(token)

	for i, arg := range cmd.Args {
		if strings.Contains(arg, token) {
			t.Fatalf("setServiceTokenEnvCommand() must never place the token in argv (Task Manager/sc qc/WMI/Sysmon all read it), but Args[%d] = %q", i, arg)
		}
	}

	stdinBytes, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("reading cmd.Stdin: %v", err)
	}
	if !strings.Contains(string(stdinBytes), token) {
		t.Fatalf("setServiceTokenEnvCommand() must deliver the token via Stdin, got %q", stdinBytes)
	}

	if cmd.Path == "" || !strings.Contains(strings.ToLower(cmd.Path), "powershell") {
		t.Errorf("setServiceTokenEnvCommand() expected a powershell.exe invocation, got Path %q", cmd.Path)
	}
}

// TestRestrictDirToSystemAdminsCommand_UsesWellKnownSIDs verifies the icacls
// invocation removes inherited permissions and grants only the well-known
// SYSTEM/Administrators SIDs (not the localized account names, which differ
// on non-English Windows) full control - P24's approved native-ACL approach
// for the Windows agent TLS key directory.
func TestRestrictDirToSystemAdminsCommand_UsesWellKnownSIDs(t *testing.T) {
	cmd := restrictDirToSystemAdminsCommand(`C:\ProgramData\ollama-mesh-agent`)

	if cmd.Args[0] != "icacls" {
		t.Fatalf("Args[0] = %q, want icacls", cmd.Args[0])
	}
	if cmd.Args[1] != `C:\ProgramData\ollama-mesh-agent` {
		t.Errorf("Args[1] = %q, want the target directory", cmd.Args[1])
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "/inheritance:r") {
		t.Error("command must remove inherited permissions (/inheritance:r)")
	}
	if !strings.Contains(joined, "S-1-5-18") {
		t.Error("command must grant the well-known SYSTEM SID (S-1-5-18)")
	}
	if !strings.Contains(joined, "S-1-5-32-544") {
		t.Error("command must grant the well-known Administrators SID (S-1-5-32-544)")
	}
	if strings.Contains(joined, "Administrators:") || strings.Contains(joined, "SYSTEM:") {
		t.Errorf("command must use well-known SIDs, not localized account names, got %q", joined)
	}
}

// TestAgentCertKeyPaths_UnderProgramData verifies Windows cert/key paths
// resolve under %ProgramData%\ollama-mesh-agent, matching the design's
// per-platform table.
func TestAgentCertKeyPaths_UnderProgramData(t *testing.T) {
	certPath, keyPath := agentCertKeyPaths()
	if !strings.HasSuffix(certPath, `ollama-mesh-agent\agent.crt`) {
		t.Errorf("certPath = %q, want to end with ollama-mesh-agent\\agent.crt", certPath)
	}
	if !strings.HasSuffix(keyPath, `ollama-mesh-agent\agent.key`) {
		t.Errorf("keyPath = %q, want to end with ollama-mesh-agent\\agent.key", keyPath)
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
