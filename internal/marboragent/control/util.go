package control

import "strings"

// splitLines splits raw command output into non-empty trimmed lines, the
// common post-processing every driver's Logs implementation needs.
func splitLines(raw string) []string {
	var lines []string
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// lastN returns the final n elements of lines (or all of them if there are
// fewer than n) - used by drivers whose native log command has no "last N
// lines" flag of its own, so the line-count contract is honored regardless
// of how the underlying tool's output is shaped.
func lastN(lines []string, n int) []string {
	if n <= 0 {
		// Contradicts "last N lines" for any N<=0 - not reachable today
		// (handleRuntimeLogs already clamps req.Lines<=0 to a default before
		// calling any driver's Logs()), guarded here for defense-in-depth
		// against a future direct caller.
		return nil
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
