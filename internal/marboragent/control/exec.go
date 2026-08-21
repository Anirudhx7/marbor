package control

import (
	"context"
	"os/exec"
)

// lookPath and runCommand are package-level seams (same pattern as
// internal/marboragent's gpu_nvidia.go var lookPath = exec.LookPath) so tests
// can simulate a tool being present/absent, or a command's output, without
// depending on what's actually installed on the machine running the test.
var lookPath = exec.LookPath

// runCommand runs name with args and returns combined stdout+stderr,
// trimmed of nothing (callers trim/parse as needed) - combined output is
// used because several of the tools this package shells out to (systemctl,
// launchctl, sc) put diagnostic detail on stderr that is useful in an error
// message, same reasoning as actions.go's runDownload.
var runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
