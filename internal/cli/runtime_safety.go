package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
)

// maxSoleQueryBytes caps the encoded query string a drain or restart sends.
// Reverse proxies often reject request lines past a few KB; a list of
// acknowledged models that big is better sent from a script with a bare force.
const maxSoleQueryBytes = 6000

// maxConflictBodyBytes caps how much of a 409 body the client keeps.
const maxConflictBodyBytes = 1 << 20

// stripControl removes control characters (escape sequences, newlines, bells)
// from a name before it is printed to a terminal, so a model or runtime name
// from a node cannot rewrite the screen. It also drops Unicode format characters
// (bidi overrides, zero-width) and line or paragraph separators. Display only:
// the names sent back in acknowledge_sole stay as they were.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, s)
}

// displayNames strips control characters from each name for printing.
func displayNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = stripControl(n)
	}
	return out
}

// soleActionQuery builds the query string for a drain or runtime action: ""
// when nothing is set, otherwise "?" plus the encoded pairs. acknowledge_sole is
// sent only with force. A nil ackSole sends no key; a non-nil one always sends
// the key, with one empty value when the list is empty, so an empty reviewed
// list is never mistaken for a bare force.
func soleActionQuery(acknowledgeReplica, force bool, ackSole []string) (string, error) {
	v := url.Values{}
	if acknowledgeReplica {
		v.Set("acknowledge_replica", "true")
	}
	if force {
		v.Set("force", "true")
		switch {
		case ackSole == nil:
		case len(ackSole) == 0:
			v.Set("acknowledge_sole", "")
		default:
			v["acknowledge_sole"] = ackSole
		}
	}
	q := v.Encode()
	if len(q) > maxSoleQueryBytes {
		return "", userErrorf("the list of models to acknowledge is too large to send in a URL (%d bytes, limit %d): pass --force to override without the list, or call the Admin API from a script", len(q), maxSoleQueryBytes)
	}
	if q == "" {
		return "", nil
	}
	return "?" + q, nil
}

// forceFlag lets "runtime drain" and "runtime restart" proceed when the node
// is the only server of a model it holds.
var forceFlag = FlagSpec{
	Name:  "force",
	Kind:  FlagBool,
	Usage: "proceed without asking when the node is the only server of a model it holds; does not check the list of models",
}

// dryRunFlag makes "runtime drain" and "runtime restart" report what the
// sole-replica check finds and change nothing.
var dryRunFlag = FlagSpec{
	Name:  "dry-run",
	Kind:  FlagBool,
	Usage: "show which models only this node can serve and exit without changing anything",
}

// soleAction is an action the sole-replica check guards.
type soleAction string

const (
	soleActionDrain   soleAction = "drain"
	soleActionRestart soleAction = "restart"
)

// soleRequest says which action the sole-replica check is for and how the
// operator asked to handle it.
type soleRequest struct {
	action soleAction
	node   string
	force  bool
	dryRun bool
}

// soleGate is the outcome of the sole-replica check run before a drain or
// runtime restart.
type soleGate struct {
	impact    *SoleReplicaImpact // nil when the check could not be run
	sendForce bool               // the action must be sent with force=true
	ack       []string           // models the operator was shown; nil sends no acknowledge_sole (a bare force)
	done      bool               // stop here and return code
	code      int
}

// gateSoleReplica runs the read-only sole-replica check for a drain or
// runtime restart and decides whether the action may be sent. A node that is
// the only server of a model needs --force, or an interactive yes; without a
// terminal it is a user error. The server enforces the same rule, so a check
// that cannot be read is noted and the action is sent for the server to decide,
// carrying --force if the operator gave it.
func gateSoleReplica(client *Client, flags *globalFlags, req soleRequest, stdout, stderr io.Writer) soleGate {
	impact, err := client.SoleReplica(req.node)
	if err != nil {
		if req.dryRun {
			return soleGate{done: true, code: reportError(err, stderr)}
		}
		fmt.Fprintf(stderr, "note: could not check whether %q is the only server of any model: %v\n", req.node, err)
		return soleGate{sendForce: req.force}
	}
	printSoleReplicaNotes(stderr, impact)
	if req.dryRun {
		return soleGate{impact: impact, done: true, code: reportDryRun(flags, req.action, req.node, impact, stdout, stderr)}
	}
	if len(impact.Models) == 0 {
		return soleGate{impact: impact}
	}
	if req.force {
		return soleGate{impact: impact, sendForce: true}
	}
	if err := confirmSoleReplica(req.action, req.node, impact, stderr); err != nil {
		return soleGate{impact: impact, done: true, code: reportError(err, stderr)}
	}
	// The operator saw exactly this list, so the server is told to check that
	// nothing else lost its last server in the meantime.
	return soleGate{impact: impact, sendForce: true, ack: soleModelNames(impact.Models)}
}

// reportSoleActionError prints an action error and returns its exit code. When
// the server refused because the acknowledged list went stale, it also prints
// the current list to stderr and says to re-run to review it; it never retries.
func reportSoleActionError(err error, stderr io.Writer) int {
	code := reportError(err, stderr)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return code
	}
	if cliErr.BodyCut {
		// The lists cannot be trusted or shown; say so rather than stay silent.
		fmt.Fprintln(stderr, "The response was cut off, so it could not be read in full. Nothing was changed. Run the command again.")
		return code
	}
	if len(cliErr.Raw) == 0 {
		return code
	}
	var body struct {
		Code           string            `json:"code"`
		Unacknowledged []string          `json:"unacknowledged"`
		SoleReplica    SoleReplicaImpact `json:"sole_replica"`
	}
	if json.NewDecoder(bytes.NewReader(cliErr.Raw)).Decode(&body) != nil || body.Code != "sole_replica" || len(body.Unacknowledged) == 0 {
		return code
	}
	fmt.Fprintln(stderr, "The models with no other server are now:")
	for _, m := range body.SoleReplica.Models {
		fmt.Fprintf(stderr, "  %s (%s)\n", stripControl(m.Model), stripControl(m.Runtime))
	}
	fmt.Fprintln(stderr, "Nothing was changed. Run the command again to review the new list.")
	return code
}

// confirmSoleReplica asks the operator to accept taking the sole models out of
// service, or fails when there is no terminal to ask on.
func confirmSoleReplica(action soleAction, node string, impact *SoleReplicaImpact, stderr io.Writer) error {
	names := soleModelNames(impact.Models)
	if !stdinIsTTY() {
		return userErrorf("refusing to %s %q without --force: it is the only node that can serve %s (no TTY to confirm)", action, node, strings.Join(displayNames(names), ", "))
	}
	// Check the size now, so the operator is not asked to confirm a list that
	// then cannot be sent.
	if _, err := soleActionQuery(false, true, names); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Node %q is the only node that can serve:\n", node)
	for _, m := range impact.Models {
		fmt.Fprintf(stderr, "  %s (%s)\n", stripControl(m.Model), stripControl(m.Runtime))
	}
	return promptProceed(stderr, soleReplicaConsequence(action, node))
}

// soleReplicaConsequence states what proceeding does, in plain terms. Neither
// action is described as permanent: a drain is undone with "runtime undrain"
// and a restart brings the runtime back on its own.
func soleReplicaConsequence(action soleAction, node string) string {
	if action == soleActionRestart {
		return fmt.Sprintf("Restarting the runtime on %q takes it offline while it restarts, and no other node can serve these models in the meantime. The check covers restarts made through marbor only.", node)
	}
	return fmt.Sprintf("Draining %q stops routing new requests to it, and no other node can serve these models until you run \"runtime undrain\". The check covers drains made through marbor only.", node)
}

func soleModelNames(models []SoleReplicaModel) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Model)
	}
	return names
}

// printSoleReplicaNotes writes the cold-load and unverifiable findings to
// stderr, so they never pollute piped stdout or --json output.
func printSoleReplicaNotes(stderr io.Writer, impact *SoleReplicaImpact) {
	for _, c := range impact.ColdLoadOnly {
		fmt.Fprintf(stderr, "note: %q is not warm on any other node; it would have to cold-load on %s (not warm there and may not be installed; fit not checked)\n", c.Model, strings.Join(c.Peers, ", "))
	}
	for _, u := range impact.Unknown {
		subject := fmt.Sprintf("node %q", u.Node)
		if u.Model != "" {
			subject += fmt.Sprintf(" for model %q", u.Model)
		}
		fmt.Fprintf(stderr, "warning: could not verify other replicas: %s: %s\n", subject, u.Reason)
	}
}

// reportDryRun prints the check result for --dry-run and returns the exit code.
func reportDryRun(flags *globalFlags, action soleAction, node string, impact *SoleReplicaImpact, stdout, stderr io.Writer) int {
	result := map[string]interface{}{"node": node, "action": action, "dry_run": true, "sole_replica": impact.normalized()}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, result); handled {
		return code
	}
	if len(impact.Models) == 0 {
		fmt.Fprintf(stdout, "%s: dry run: no model is left without another server by a %s (nothing changed)\n", node, action)
		return ExitOK
	}
	fmt.Fprintf(stdout, "%s: dry run: a %s needs --force, it is the only node that can serve %s (nothing changed)\n", node, action, strings.Join(soleModelNames(impact.Models), ", "))
	return ExitOK
}

// normalized returns the impact with every list non-nil, so JSON output shows
// [] rather than null.
func (i SoleReplicaImpact) normalized() SoleReplicaImpact {
	if i.Models == nil {
		i.Models = []SoleReplicaModel{}
	}
	if i.ColdLoadOnly == nil {
		i.ColdLoadOnly = []ColdLoadModel{}
	}
	if i.Unknown == nil {
		i.Unknown = []SoleReplicaUnknown{}
	}
	return i
}

// soleReplicaForOutput picks the sole_replica detail to show: the server's own
// (it carries forced) when present, otherwise the pre-check, marked forced when
// force was sent for a node that had sole models. It is nil when there is
// nothing to report, so --json omits the key the way the server does. Lists are
// never null.
func soleReplicaForOutput(server *SoleReplicaBody, pre *SoleReplicaImpact, sentForce bool) *SoleReplicaBody {
	switch {
	case server != nil:
		out := *server
		out.SoleReplicaImpact = out.SoleReplicaImpact.normalized()
		return &out
	case pre != nil && len(pre.Models)+len(pre.ColdLoadOnly)+len(pre.Unknown) > 0:
		return &SoleReplicaBody{SoleReplicaImpact: pre.normalized(), Forced: sentForce && len(pre.Models) > 0}
	}
	return nil
}
