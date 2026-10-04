package admin

import (
	"testing"

	"github.com/Anirudhx7/marbor/internal/bootstrapcred"
)

// testBootstrapPassword is the operator-supplied first-boot password used by
// tests that need to log in through the real login path.
const testBootstrapPassword = "Test-Bootstrap-Pw-2026"

// envFrom returns a getenv function backed by a fixed map.
func envFrom(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// testBootstrapOptions supplies testBootstrapPassword through the environment
// source and gives the server a private data directory.
func testBootstrapOptions(t *testing.T) BootstrapOptions {
	t.Helper()
	return BootstrapOptions{
		DataDir: t.TempDir(),
		Getenv:  envFrom(map[string]string{bootstrapcred.EnvPassword: testBootstrapPassword}),
	}
}
