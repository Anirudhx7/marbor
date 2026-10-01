package cli

// model_aliases.go - `marbor models alias list|set|remove`, the CLI peer of
// the Routing page's "Model aliases" section, over GET/PUT/DELETE
// /admin/model-aliases.

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// aliasAvailability renders the target column honestly: "not checked" when
// the fleet inventory could not be read, never a guessed yes/no.
func aliasAvailability(a ModelAlias) string {
	if !a.InventoryChecked {
		return "not checked"
	}
	if a.TargetAvailable {
		return a.TargetStatus
	}
	return "not on fleet"
}

func aliasShadowWarning(a ModelAlias) string {
	return fmt.Sprintf("warning: %q is also a model currently on the fleet; the alias wins, so requests for it are served by %q", a.Alias, a.Target)
}

// runModelsAliasList implements `marbor models alias list`.
func runModelsAliasList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	aliases, err := client.ListModelAliases()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, aliases); handled {
		return code
	}
	if len(aliases) == 0 {
		fmt.Fprintln(stdout, "no model aliases configured")
		return ExitOK
	}
	tw := newTabWriter(stdout)
	fmt.Fprintln(tw, "ALIAS\tTARGET\tTARGET STATUS\tSHADOWS MODEL")
	for _, a := range aliases {
		shadows := "no"
		switch {
		case !a.InventoryChecked:
			shadows = "not checked"
		case a.ShadowsModel:
			shadows = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Alias, a.Target, aliasAvailability(a), shadows)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitServerError
	}
	for _, a := range aliases {
		if a.ShadowsModel {
			fmt.Fprintln(stderr, aliasShadowWarning(a))
		}
	}
	return ExitOK
}

// runModelsAliasSet implements `marbor models alias set <alias> <target>`.
func runModelsAliasSet(flags *globalFlags, alias, target string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	a, err := client.SetModelAlias(alias, target)
	if err != nil {
		return reportError(err, stderr)
	}
	if a.ShadowsModel {
		fmt.Fprintln(stderr, aliasShadowWarning(a))
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, a); handled {
		return code
	}
	fmt.Fprintf(stdout, "alias %q -> %q saved (target: %s)\n", a.Alias, a.Target, aliasAvailability(a))
	return ExitOK
}

// confirmAliasRemoval asks before removing an alias. Removal is reversible
// (re-add the alias), but clients still using the name start failing
// immediately, so it is confirmed like any other disruptive action.
func confirmAliasRemoval(alias string, yes bool, stderr io.Writer) error {
	if yes {
		return nil
	}
	if !stdinIsTTY() {
		return userErrorf("refusing to remove model alias %q without --yes (no TTY to confirm)", alias)
	}
	fmt.Fprintf(stderr, "Clients requesting %q will stop reaching its target and their requests will fail until they switch names or you re-add the alias.\nRemove model alias %q? [y/N] ", alias, alias)
	line, err := bufio.NewReader(stdinReader).ReadString('\n')
	if err != nil && len(line) == 0 {
		return userErrorf("aborted: could not read confirmation")
	}
	if ans := strings.TrimSpace(strings.ToLower(line)); ans == "y" || ans == "yes" {
		return nil
	}
	return userErrorf("aborted")
}

// runModelsAliasRemove implements `marbor models alias remove <alias> [--yes]`.
func runModelsAliasRemove(flags *globalFlags, alias string, yes bool, stdout, stderr io.Writer) int {
	if err := confirmAliasRemoval(alias, yes, stderr); err != nil {
		return reportError(err, stderr)
	}
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.DeleteModelAlias(alias); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "alias": alias, "removed": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "model alias %q removed\n", alias)
	return ExitOK
}
