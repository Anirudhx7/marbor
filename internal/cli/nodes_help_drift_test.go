package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
)

// The nodes help lists the signal ids by hand; this fails when the server
// adds, renames or removes one and the help text was not updated.
func TestNodesHelpListsEveryServerSignalID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	Run([]string{"nodes", "--help"}, &stdout, &stderr)
	help := stdout.String() + stderr.String()
	if !strings.Contains(help, "--signal") {
		t.Fatalf("nodes help not rendered: %q", help)
	}
	for _, id := range admin.NodeSignalIDs() {
		if !strings.Contains(help, id) {
			t.Errorf("nodes help does not mention signal %q", id)
		}
	}
}
