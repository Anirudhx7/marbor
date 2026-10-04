package cli

import (
	"fmt"
	"io"
	"time"
)

// runStatus implements `marbor status` - GET /health, unauthenticated.
func runStatus(flags *globalFlags, stdout, stderr io.Writer) int {
	client := NewClient(flags.server, "")
	health, err := client.Health()
	if err != nil {
		return reportError(err, stderr)
	}

	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, health); handled {
		return code
	}

	uptime := time.Duration(health.UptimeSeconds) * time.Second
	fmt.Fprintf(stdout, "status:   %s\n", health.Status)
	fmt.Fprintf(stdout, "version:  %s\n", health.Version)
	fmt.Fprintf(stdout, "uptime:   %s\n", uptime)
	fmt.Fprintf(stdout, "proxy:    port %d\n", health.ProxyPort)
	fmt.Fprintf(stdout, "nodes:    %d/%d healthy\n", health.Nodes.Healthy, health.Nodes.Total)
	if health.BootstrapPasswordPending {
		fmt.Fprintf(stdout, "login:    the initial admin password is still unchanged - %s\n", pendingPasswordAdvice(health.BootstrapPasswordSource))
	}
	return ExitOK
}

// pendingPasswordAdvice tells the operator how to sign in for the first time,
// by where the pending password comes from. It never names a password; the
// default case names the public factory login, which is not a secret.
func pendingPasswordAdvice(source string) string {
	switch source {
	case "file":
		return "sign in with the generated password in the file initial-admin-password next to marbor.db, then change it"
	case "supplied":
		return "sign in with the password you configured, then change it"
	case "default":
		return "sign in with the default admin credentials and change the password now"
	}
	return "log in and change it"
}
