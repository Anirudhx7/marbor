package hygiene

// go_toolchain_pin_test.go: release builds must use one exact Go patch
// version so a release is reproducible and a toolchain move is a reviewed
// change. The pin lives in several files; this keeps them in agreement.

import (
	"regexp"
	"strings"
	"testing"
)

var (
	toolchainLine = regexp.MustCompile(`(?m)^toolchain go(1\.\d+\.\d+)\s*$`)
	goImageTag    = regexp.MustCompile(`(?m)^FROM (?:--platform=\S+ )?golang:(\S+)`)
	exactGoTag    = regexp.MustCompile(`^(1\.\d+\.\d+)(-alpine)?$`)
)

func TestGoToolchainPinnedAndConsistent(t *testing.T) {
	m := toolchainLine.FindStringSubmatch(readRepoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no exact `toolchain goX.Y.Z` line")
	}
	pin := m[1]
	for _, f := range []string{"Dockerfile", "demo/Dockerfile.mocknode", "demo/Dockerfile.demotraffic"} {
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

func TestReleaseWorkflowsUseThePinnedToolchain(t *testing.T) {
	for _, f := range []string{".github/workflows/release.yml", ".github/workflows/release-check.yml"} {
		text := readRepoFile(t, f)
		if !strings.Contains(text, "go-version-file: go.mod") {
			t.Errorf("%s: setup-go must read the Go version from go.mod", f)
		}
		if strings.Contains(text, "check-latest") {
			t.Errorf("%s: release builds must not float to the latest Go", f)
		}
		if regexp.MustCompile(`go-version:`).MatchString(text) {
			t.Errorf("%s: do not set go-version beside go-version-file", f)
		}
	}
}
