package hygiene

// go_toolchain_pin_test.go: release builds must use one exact Go patch
// version so a release is reproducible and a toolchain move is a reviewed
// change. The pin lives in several files; this keeps them in agreement.

import (
	"path/filepath"
	"regexp"
	"testing"
)

var (
	toolchainLine = regexp.MustCompile(`(?m)^toolchain go(1\.\d+\.\d+)\s*$`)
	goImageTag    = regexp.MustCompile(`(?mi)^FROM (?:--\S+ )*golang:(\S+)`)
	exactGoTag    = regexp.MustCompile(`^(1\.\d+\.\d+)(-[a-z0-9.]+)?(@sha256:[0-9a-f]+)?$`)
	setupGoFile   = regexp.MustCompile(`(?m)^\s*go-version-file: go\.mod\s*$`)
	setupGoFloat  = regexp.MustCompile(`(?m)^\s*(go-version|check-latest):`)
	setupGoStep   = regexp.MustCompile(`(?m)^\s*- uses: actions/setup-go@`)
)

func TestGoToolchainPinnedAndConsistent(t *testing.T) {
	m := toolchainLine.FindStringSubmatch(readRepoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no exact `toolchain goX.Y.Z` line")
	}
	pin := m[1]
	demo, err := filepath.Glob("../../demo/Dockerfile.*")
	if err != nil || len(demo) == 0 {
		t.Fatalf("no demo Dockerfiles found: %v", err)
	}
	files := []string{"Dockerfile"}
	for _, d := range demo {
		files = append(files, "demo/"+filepath.Base(d))
	}
	for _, f := range files {
		tags := goImageTag.FindAllStringSubmatch(readRepoFile(t, f), -1)
		if len(tags) == 0 {
			t.Errorf("%s: no golang image stage found", f)
		}
		for _, tag := range tags {
			em := exactGoTag.FindStringSubmatch(tag[1])
			if em == nil {
				t.Errorf("%s: golang:%s is not an exact patch tag", f, tag[1])
			} else if em[1] != pin {
				t.Errorf("%s: golang:%s differs from go.mod toolchain go%s", f, tag[1], pin)
			}
		}
	}
}

// Only the release workflows are checked: regular CI deliberately tests the
// latest patch of the Go minor. The step regexp assumes the `- uses:` form.
func TestReleaseWorkflowsUseThePinnedToolchain(t *testing.T) {
	for _, f := range []string{".github/workflows/release.yml", ".github/workflows/release-check.yml"} {
		text := readRepoFile(t, f)
		steps := len(setupGoStep.FindAllString(text, -1))
		if steps == 0 {
			t.Errorf("%s: no setup-go step found", f)
		}
		if got := len(setupGoFile.FindAllString(text, -1)); got != steps {
			t.Errorf("%s: %d setup-go steps but %d read the Go version from go.mod", f, steps, got)
		}
		if setupGoFloat.MatchString(text) {
			t.Errorf("%s: release builds must not set go-version or check-latest", f)
		}
	}
}
