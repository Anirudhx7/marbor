package cli

// help_golden_test.go captures byte-for-byte output of the CLI's help/usage
// surfaces so a later refactor (routing help through the registry-backed
// writeHelp, see help.go) can be verified against pre-refactor behavior. See
// .local/plans/reflective-pondering-acorn.md, "Implementation" section 3 and
// migration step 4.
//
// Run with -update to (re)write the golden files under testdata/help/:
//
//	go test ./internal/cli/... -run TestHelpGoldens -update
//
// Without -update, the test compares current output against the committed
// goldens and fails with a readable diff on any mismatch.

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGoldens = flag.Bool("update", false, "update golden files in testdata/help")

func goldenPath(name string) string {
	return filepath.Join("testdata", "help", name+".golden")
}

// checkGolden compares got against the golden file for name, or (with
// -update) writes got as the new golden.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata/help: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create it first): %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("output for %q does not match golden %s\n--- want ---\n%s\n--- got ---\n%s", name, path, want, got)
	}
}

// TestHelpGoldens pins the current byte-for-byte output of every help/usage
// surface in the CLI package: root --help (now rendered by the same
// registry-backed writeHelp/writeRootHelp path as every group/leaf, since
// Fix 3 of the P83+ CLI hardening code review deleted the old hand-aligned
// `usage` const in cli.go - the golden below was regenerated for the new,
// correctly-aligned output, an intentional and reviewed change), all six
// existing print*Usage functions, and (informationally only - this one is
// EXPECTED to change once the usage=nil commands are migrated to
// registry-backed help in a later step) the raw flag.FlagSet.Usage fallback
// used today by commands that pass usage=nil into parseFlags.
func TestHelpGoldens(t *testing.T) {
	t.Run("top-level", func(t *testing.T) {
		var buf bytes.Buffer
		writeHelp(&buf, root())
		checkGolden(t, "top-level", buf.Bytes())
	})

	t.Run("models", func(t *testing.T) {
		var buf bytes.Buffer
		printModelsUsage(&buf)
		checkGolden(t, "models", buf.Bytes())
	})

	t.Run("runtime", func(t *testing.T) {
		var buf bytes.Buffer
		printRuntimeUsage(&buf)
		checkGolden(t, "runtime", buf.Bytes())
	})

	t.Run("login", func(t *testing.T) {
		var buf bytes.Buffer
		printLoginUsage(&buf)
		checkGolden(t, "login", buf.Bytes())
	})

	t.Run("node-control", func(t *testing.T) {
		var buf bytes.Buffer
		printNodeControlUsage(&buf)
		checkGolden(t, "node-control", buf.Bytes())
	})

	t.Run("key", func(t *testing.T) {
		var buf bytes.Buffer
		printKeyUsage(&buf)
		checkGolden(t, "key", buf.Bytes())
	})

	t.Run("requests", func(t *testing.T) {
		var buf bytes.Buffer
		printRequestsUsage(&buf)
		checkGolden(t, "requests", buf.Bytes())
	})

	// Informational only: today six of ~twelve command groups pass
	// usage=nil into parseFlags, which falls back to the raw
	// flag.FlagSet.Usage() - a bare "Usage of <name>:" plus PrintDefaults,
	// with no command description. "whoami" is one such command. This
	// golden exists so a future migration of whoami onto registry-backed
	// help (writeHelp) can show, by diffing against this file, exactly how
	// much the raw fallback output improves - it is NOT a compatibility
	// contract like the six print*Usage goldens above, and is expected to
	// change/improve.
	t.Run("whoami-raw-fallback", func(t *testing.T) {
		var buf bytes.Buffer
		fs, _ := newFlagSet("whoami", &buf)
		fs.SetOutput(&buf)
		fs.Usage()
		checkGolden(t, "whoami-raw-fallback", buf.Bytes())
	})
}
