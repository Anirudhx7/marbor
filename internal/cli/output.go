package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// writeJSON encodes v as indented JSON to w. Used by every command's --json
// path - this is the actual compatibility contract, not the human table
// output below.
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

// emitRawJSONList is emitJSON for a list response body the CLI passes through
// as received, re-indented, instead of re-encoding a typed copy that would
// drop any field the CLI does not mirror. A null body prints as [] so a list
// command keeps its array shape. The body must already be valid JSON.
func emitRawJSONList(stdout, stderr io.Writer, jsonOutput bool, raw json.RawMessage) (bool, int) {
	if !jsonOutput {
		return false, 0
	}
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		trimmed = []byte("[]")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		fmt.Fprintf(stderr, "could not format the server response: %v\n", err)
		return true, ExitServerError
	}
	buf.WriteByte('\n')
	if _, err := stdout.Write(buf.Bytes()); err != nil {
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

// ptrIf returns &v when set is true, nil otherwise - the shared shape of a
// "visited flag becomes an optional patch pointer" assignment (RunCtx.IsSet
// -> *T), used by every PATCH-style command (schedules patch, nodes patch)
// so each visited-flag block is one line instead of a hand-copied
// if/declare/assign (code review finding).
func ptrIf[T any](set bool, v T) *T {
	if !set {
		return nil
	}
	return &v
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
