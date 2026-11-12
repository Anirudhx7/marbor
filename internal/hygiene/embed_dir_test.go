package hygiene

// embed_dir_test.go: the admin package embeds web/dist, which is gitignored
// build output. Any context that compiles Go on a clean checkout (CI job, make
// target, image build) fails with "pattern web/dist: no matching files found"
// unless it first creates a stub dir (check contexts) or builds the real UI
// (ship contexts). This has bitten three separate contexts.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const embedDir = "web/dist"

// stubLine matches a real command creating the dir, not a comment naming it.
var stubLine = regexp.MustCompile(`(?m)^\s*@?(mkdir -p|touch) internal/admin/web/dist`)

var (
	jobHeader  = regexp.MustCompile(`(?m)^  ([a-z0-9-]+):\s*$`)
	compilesGo = regexp.MustCompile(`\bgo (vet|test|build)\b|scripts/smoke\.sh`)
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// splitJobs returns the text of each job under the top-level jobs: key.
func splitJobs(t *testing.T, workflow string) map[string]string {
	t.Helper()
	_, body, ok := strings.Cut(workflow, "\njobs:\n")
	if !ok {
		t.Fatal("no jobs: key found")
	}
	idx := jobHeader.FindAllStringSubmatchIndex(body, -1)
	jobs := map[string]string{}
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		jobs[body[m[2]:m[3]]] = body[m[0]:end]
	}
	return jobs
}

func TestGoCompilingContextsProvideTheEmbedDir(t *testing.T) {
	jobs := splitJobs(t, readRepoFile(t, ".github/workflows/ci.yml"))
	if len(jobs) < 3 {
		t.Fatalf("parsed only %d CI jobs; job splitter is broken", len(jobs))
	}
	for name, text := range jobs {
		if compilesGo.MatchString(text) && !stubLine.MatchString(text) {
			t.Errorf("ci.yml job %q compiles Go but never creates %s (add the stub step)", name, embedDir)
		}
	}
	makefile := readRepoFile(t, "Makefile")
	_, testTarget, _ := strings.Cut(makefile, "\ntest:")
	testTarget, _, _ = strings.Cut(testTarget, "\n\n")
	if !stubLine.MatchString(testTarget) {
		t.Errorf("Makefile test target runs go test without creating %s", embedDir)
	}
	// Ship context: the image must carry the real UI, never a stub.
	dockerfile := readRepoFile(t, "Dockerfile")
	copyUI := strings.Index(dockerfile, "COPY --from=ui")
	build := strings.Index(dockerfile, "go build")
	if copyUI < 0 || build < 0 || copyUI > build {
		t.Errorf("Dockerfile must COPY the built UI (--from=ui) before go build")
	}
}
