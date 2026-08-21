//go:build windows

package service

import (
	"strings"
	"testing"
)

// TestSetServiceTokenEnvCommandUsesMARBOR_AGENT_SECRET verifies the Windows
// service installer writes the new Marbor Agent contract variable
// (MARBOR_AGENT_SECRET) into the service's Environment registry value - not
// the legacy TOKEN key. The agent binary only reads MARBOR_AGENT_SECRET, so a
// service that wrote TOKEN would start the agent with an empty secret.
func TestSetServiceTokenEnvCommandUsesMARBOR_AGENT_SECRET(t *testing.T) {
	const token = "sekret-marbor-agent-token"
	cmd := setServiceTokenEnvCommand(token)

	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "TOKEN=") {
		t.Errorf("setServiceTokenEnvCommand script must not contain legacy TOKEN= key: %s", joined)
	}
	if !strings.Contains(joined, "MARBOR_AGENT_SECRET=$t") {
		t.Errorf("setServiceTokenEnvCommand script missing MARBOR_AGENT_SECRET=$t: %s", joined)
	}
	// Token must not appear in argv - it travels via stdin only.
	for i, arg := range cmd.Args {
		if strings.Contains(arg, token) {
			t.Fatalf("setServiceTokenEnvCommand must never place the token in argv (Task Manager/sc qc/WMI/Sysmon all read it), but Args[%d] = %q", i, arg)
		}
	}
}
