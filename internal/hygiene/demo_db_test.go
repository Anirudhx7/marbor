package hygiene

// demo_db_test.go keeps the standalone demo compose file honest. It carries
// its own base64 copy of demo/marbor.demo.db (so it works with no checkout),
// and nothing else ties that copy to the tracked fixture. A stale copy would
// show the public demo an old schema, so the two must decode to the same bytes.

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneComposeDemoDBMatchesFixture(t *testing.T) {
	root := filepath.Join("..", "..")
	compose, err := os.ReadFile(filepath.Join(root, "demo", "docker-compose.standalone.yml"))
	if err != nil {
		t.Fatalf("reading standalone compose: %v", err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "demo", "marbor.demo.db"))
	if err != nil {
		t.Fatalf("reading demo fixture: %v", err)
	}

	var b64 strings.Builder
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(string(compose), "\r\n", "\n"), "\n") {
		switch {
		case line == "  demo_marbor_db:":
			in = true
		case in && strings.HasPrefix(line, "    content:"):
		case in && strings.HasPrefix(line, "      "):
			b64.WriteString(strings.TrimSpace(line))
		case in:
			in = false
		}
	}
	if b64.Len() == 0 {
		t.Fatal("no inline demo_marbor_db content found in demo/docker-compose.standalone.yml")
	}
	got, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("inline demo database is not valid base64: %v", err)
	}
	if !bytes.Equal(got, fixture) {
		t.Fatal("demo/docker-compose.standalone.yml carries a different database than demo/marbor.demo.db - run 'make demo-db' and commit both")
	}
}
