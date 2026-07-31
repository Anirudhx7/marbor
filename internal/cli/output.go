package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// writeJSON encodes v as indented JSON to w. Used by every command's --json
// path - this is the actual compatibility contract (operational-interfaces.md
// 5.1), not the human table output below.
func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emitJSON writes v as JSON to stdout when jsonOutput is set. It returns
// (true, exitCode) when it handled the output (the caller should return
// exitCode immediately), or (false, 0) when the caller should render its
// human table instead. Centralizes the json-or-table branch every command
// needs so it can't drift per command.
func emitJSON(stdout, stderr io.Writer, jsonOutput bool, v interface{}) (bool, int) {
	if !jsonOutput {
		return false, 0
	}
	if err := writeJSON(stdout, v); err != nil {
		fmt.Fprintln(stderr, err)
		return true, ExitServerError
	}
	return true, ExitOK
}

// newTabWriter returns a tabwriter configured the same way for every
// command's human-readable table output.
func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func fmtMB(mb int64) string {
	return fmt.Sprintf("%d MB", mb)
}
