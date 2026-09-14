package hygiene

// binary_separation_test.go is the standing guard for the permanent product
// promise that marbor (control plane + CLI) and marbor-agent are two static
// binaries,
// neither capable of the other's role. That separation holds today by
// inspection only - nothing previously stopped it from silently breaking
// as the codebase grows. This test checks the real, transitive Go import
// graph (via `go list -deps`, not a text grep of import blocks), so an
// indirect leak through some future helper package is caught too, not just
// a direct import.

import (
	"os/exec"
	"strings"
	"testing"
)

// controlPlanePackages are the packages that make a binary capable of
// running the control plane (admin API, proxy, router, auth, CLI). A GPU/
// node-agent host must never carry an executable that imports any of
// these - that is the entire reason marbor-agent was split into its own
// binary.
var controlPlanePackages = []string{
	"github.com/Anirudhx7/marbor/internal/admin",
	"github.com/Anirudhx7/marbor/internal/auth",
	"github.com/Anirudhx7/marbor/internal/router",
	"github.com/Anirudhx7/marbor/internal/proxy",
	"github.com/Anirudhx7/marbor/internal/cli",
}

// The reverse direction - the control-plane binary becoming capable of
// running as marbor-agent - does not need an equivalent test here. The
// actual agent runtime lives in cmd/marbor-agent, a `package main` that Go
// refuses to let anything import at all, so the compiler itself already
// makes that direction impossible. internal/admin does legitimately import
// internal/marboragent today (admin.go), but only for shared type
// definitions (marboragent.GPUInfo, marboragent.ScopeAdmin) used to
// describe agent-reported state in the admin API - not for anything that
// starts an agent server. That is a deliberate, existing design choice
// (shared types are fine, shared server-starting capability is not), so a
// blanket "main must never import internal/marboragent" check would fail
// on legitimate code rather than catching a real regression.

// goListDeps returns the transitive dependency import paths of pkg (as
// reported by `go list -deps`, one path per line), including pkg's own
// direct and indirect standard-library and module dependencies.
func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

// TestMarborAgentNeverImportsControlPlanePackages fails if cmd/marbor-agent
// or internal/marboragent (including its subpackages) ever comes to
// transitively depend on any control-plane package - the exact silent
// regression this test exists to catch before it becomes a security-
// boundary break rather than a cosmetic one.
func TestMarborAgentNeverImportsControlPlanePackages(t *testing.T) {
	for _, pkg := range []string{
		"github.com/Anirudhx7/marbor/cmd/marbor-agent",
		"github.com/Anirudhx7/marbor/internal/marboragent/...",
	} {
		deps := goListDeps(t, pkg)
		for _, dep := range deps {
			for _, forbidden := range controlPlanePackages {
				if dep == forbidden {
					t.Errorf("%s transitively imports %s - marbor-agent must never be capable of the control plane's role", pkg, dep)
				}
			}
		}
	}
}
