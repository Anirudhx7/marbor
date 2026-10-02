package cli

// replica_suggestions.go - `marbor nodes suggestions`: list the multi-host
// replica groups the agents' launch evidence suggests, and confirm, dismiss or
// restore one. Each command is exactly one Admin API request (or a list read
// first, so confirm can name the nodes it is about to change).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// NodeSuggestionEvidence mirrors one evidence entry of a replica suggestion.
type NodeSuggestionEvidence struct {
	Node   string `json:"node"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

// NodeSuggestionDeclared mirrors a member's own declared replica_peers.
type NodeSuggestionDeclared struct {
	Node    string   `json:"node"`
	Members []string `json:"members"`
	Head    string   `json:"head"`
}

// ReplicaSuggestion mirrors one element of GET /admin/replica-suggestions.
type ReplicaSuggestion struct {
	Fingerprint string                   `json:"fingerprint"`
	Launcher    string                   `json:"launcher"`
	Runtime     string                   `json:"runtime"`
	State       string                   `json:"state"`
	Reason      string                   `json:"reason"`
	Head        string                   `json:"head"`
	Members     []string                 `json:"members"`
	Evidence    []NodeSuggestionEvidence `json:"evidence"`
	Missing     []string                 `json:"missing"`
	Declared    []NodeSuggestionDeclared `json:"declared"`
	Confirmable bool                     `json:"confirmable"`
	Dismissed   bool                     `json:"dismissed"`
}

// TopologyCoverage mirrors one coverage row: whether detection can see a node.
type TopologyCoverage struct {
	Node     string `json:"node"`
	Host     string `json:"host"`
	State    string `json:"state"`
	Detected bool   `json:"detected"`
	Detail   string `json:"detail"`
}

// ReplicaSuggestionsResponse mirrors GET /admin/replica-suggestions.
type ReplicaSuggestionsResponse struct {
	Suggestions    []ReplicaSuggestion `json:"suggestions"`
	Coverage       []TopologyCoverage  `json:"coverage"`
	DismissedCount int                 `json:"dismissedCount"`
}

// suggestionRequest sends one authenticated request to a replica-suggestion
// route. A 400, 404, 409 or 422 is the operator's to act on (stale group,
// incomplete group, contradicting declaration), so it exits as a user error
// with the server's own sentence.
func (c *Client) suggestionRequest(method, fingerprint, action string) ([]byte, error) {
	if c.Token == "" {
		return nil, userErrorf("authentication required: run marbor login, or pass --username/--password (or MARBOR_USERNAME+MARBOR_PASSWORD)")
	}
	req, err := http.NewRequest(method, c.BaseURL+"/admin/v1/replica-suggestions/"+urlPathEscape(fingerprint)+"/"+action, nil)
	if err != nil {
		return nil, userErrorf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, serverErrorf("could not reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, authErrorf("%s%s", readErrorMessage(resp.Body), c.savedSessionHint())
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, userErrorf("%s", readErrorMessage(resp.Body))
	case resp.StatusCode >= 400:
		return nil, serverErrorf("server error (%d): %s", resp.StatusCode, readErrorMessage(resp.Body))
	}
	return io.ReadAll(resp.Body)
}

// ReplicaSuggestions calls GET /admin/v1/replica-suggestions.
func (c *Client) ReplicaSuggestions(includeDismissed bool) (ReplicaSuggestionsResponse, error) {
	path := "/admin/v1/replica-suggestions"
	if includeDismissed {
		path += "?includeDismissed=true"
	}
	var out ReplicaSuggestionsResponse
	resp, err := c.doRequest(http.MethodGet, path, true)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, serverErrorf("decode replica suggestions: %v", err)
	}
	return out, nil
}

func runNodesSuggestions(ctx *RunCtx) int {
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	resp, err := client.ReplicaSuggestions(ctx.Bool("all"))
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	if handled, code := emitJSON(ctx.Stdout, ctx.Stderr, ctx.Flags.jsonOutput, resp); handled {
		return code
	}

	if len(resp.Suggestions) == 0 {
		fmt.Fprintln(ctx.Stdout, "No replica suggestions.")
	} else {
		tw := newTabWriter(ctx.Stdout)
		fmt.Fprintln(tw, "ID\tSTATE\tHEAD\tMEMBERS\tLAUNCHER\tNOTE")
		for _, s := range resp.Suggestions {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Fingerprint, suggestionStateCell(s), dashIfEmpty(s.Head),
				strings.Join(s.Members, ","), dashIfEmpty(s.Launcher), suggestionNote(s))
		}
		if err := tw.Flush(); err != nil {
			fmt.Fprintln(ctx.Stderr, err)
			return ExitServerError
		}
	}
	if resp.DismissedCount > 0 && !ctx.Bool("all") {
		fmt.Fprintf(ctx.Stdout, "%d dismissed (use --all to show them)\n", resp.DismissedCount)
	}
	printSuggestionCoverage(ctx.Stdout, resp.Coverage)
	return ExitOK
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func suggestionStateCell(s ReplicaSuggestion) string {
	if s.Dismissed {
		return s.State + " (dismissed)"
	}
	return s.State
}

// suggestionNote is the one-line explanation shown next to a suggestion.
func suggestionNote(s ReplicaSuggestion) string {
	switch {
	case s.Confirmable:
		return "ready: marbor nodes suggestions confirm " + s.Fingerprint
	case s.Reason != "":
		return s.Reason
	}
	return "-"
}

// printSuggestionCoverage lists only the nodes where multi-host detection
// cannot see anything, with the reason; reporting nodes are not repeated.
func printSuggestionCoverage(w io.Writer, cov []TopologyCoverage) {
	var lines []string
	for _, c := range cov {
		if c.State == "reporting" {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", c.Node, c.Detail))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNot reporting multi-host launch details:")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// findSuggestion returns the listed suggestion with the given fingerprint.
func findSuggestion(list []ReplicaSuggestion, fingerprint string) (ReplicaSuggestion, bool) {
	for _, s := range list {
		if s.Fingerprint == fingerprint {
			return s, true
		}
	}
	return ReplicaSuggestion{}, false
}

// confirmReplicaPrompt asks before declaring a replica group. It is its own
// text rather than requireConfirm's, because this change is reversible and the
// operator needs to know exactly which nodes change role.
func confirmReplicaPrompt(s ReplicaSuggestion, yes bool, stderr io.Writer) error {
	if yes {
		return nil
	}
	if !stdinIsTTY() {
		return userErrorf("refusing to declare replica group %s without --yes (no TTY to confirm)", s.Fingerprint)
	}
	var workers []string
	for _, m := range s.Members {
		if m != s.Head {
			workers = append(workers, m)
		}
	}
	fmt.Fprintf(stderr,
		"This declares one replica group: head %q, workers %s.\n"+
			"Marbor will route requests for this group to the head only; the workers stop receiving direct traffic.\n"+
			"Nothing is restarted and in-flight requests are unaffected. To undo it, clear replica membership on each\n"+
			"node (marbor nodes patch <node> --replica-members \"\").\n"+
			"Proceed? [y/N] ", s.Head, strings.Join(workers, ", "))
	br := bufio.NewReader(stdinReader)
	line, err := br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return userErrorf("aborted: could not read confirmation")
	}
	ans := strings.TrimSpace(strings.ToLower(line))
	if ans == "y" || ans == "yes" {
		return nil
	}
	return userErrorf("aborted")
}

func runNodesSuggestionsConfirm(ctx *RunCtx, fingerprint string) int {
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	// Read the suggestion first so the prompt can name what will change. The
	// server recomputes and re-checks everything on the confirm itself.
	resp, err := client.ReplicaSuggestions(true)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	s, ok := findSuggestion(resp.Suggestions, fingerprint)
	if !ok {
		fmt.Fprintf(ctx.Stderr, "error: no current suggestion %q (list them with: marbor nodes suggestions --all)\n", fingerprint)
		return ExitUserError
	}
	if !s.Confirmable {
		reason := s.Reason
		if reason == "" {
			reason = "not complete"
		}
		fmt.Fprintf(ctx.Stderr, "error: suggestion %s is %s and cannot be confirmed: %s\n", fingerprint, s.State, reason)
		return ExitUserError
	}
	if err := confirmReplicaPrompt(s, ctx.Bool("yes"), ctx.Stderr); err != nil {
		return reportError(err, ctx.Stderr)
	}
	body, err := client.suggestionRequest(http.MethodPost, fingerprint, "confirm")
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	if ctx.Flags.jsonOutput {
		var out any
		if err := json.Unmarshal(body, &out); err != nil {
			fmt.Fprintf(ctx.Stderr, "error: decode response: %v\n", err)
			return ExitServerError
		}
		if handled, code := emitJSON(ctx.Stdout, ctx.Stderr, true, out); handled {
			return code
		}
	}
	var done struct {
		Head  string `json:"head"`
		Roles []struct {
			Node string `json:"node"`
			Role string `json:"role"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(body, &done); err != nil {
		fmt.Fprintf(ctx.Stderr, "error: decode response: %v\n", err)
		return ExitServerError
	}
	fmt.Fprintf(ctx.Stdout, "replica group declared: head %q\n", done.Head)
	for _, r := range done.Roles {
		fmt.Fprintf(ctx.Stdout, "  %s: %s\n", r.Node, r.Role)
	}
	return ExitOK
}

func runNodesSuggestionsDismiss(ctx *RunCtx, fingerprint string, dismiss bool) int {
	client, err := authenticatedClient(ctx.Flags)
	if err != nil {
		return reportError(err, ctx.Stderr)
	}
	method, verb := http.MethodPost, "dismissed"
	if !dismiss {
		method, verb = http.MethodDelete, "restored"
	}
	if _, err := client.suggestionRequest(method, fingerprint, "dismiss"); err != nil {
		return reportError(err, ctx.Stderr)
	}
	if handled, code := emitJSON(ctx.Stdout, ctx.Stderr, ctx.Flags.jsonOutput, map[string]any{
		"ok": true, "fingerprint": fingerprint, "dismissed": dismiss,
	}); handled {
		return code
	}
	fmt.Fprintf(ctx.Stdout, "suggestion %s %s\n", fingerprint, verb)
	return ExitOK
}
