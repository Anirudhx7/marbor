package cli

// favorites.go - `marbor favorites list/add/remove` (GET/POST/DELETE
// /admin/favorites had full UI coverage in ModelAdvisor.tsx but no CLI).

import (
	"fmt"
	"io"
)

func printFavoritesUsage(w io.Writer) { writeHelp(w, findCommand(root(), "favorites")) }

// runFavoritesList implements `marbor favorites list`.
func runFavoritesList(flags *globalFlags, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	ids, err := client.Favorites()
	if err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"model_ids": ids}); handled {
		return code
	}
	for _, id := range ids {
		fmt.Fprintln(stdout, id)
	}
	return ExitOK
}

// runFavoritesAdd implements `marbor favorites add <model-id>`.
func runFavoritesAdd(flags *globalFlags, modelID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.AddFavorite(modelID); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "model_id": modelID, "starred": true}); handled {
		return code
	}
	fmt.Fprintf(stdout, "%q starred\n", modelID)
	return ExitOK
}

// runFavoritesRemove implements `marbor favorites remove <model-id>`.
func runFavoritesRemove(flags *globalFlags, modelID string, stdout, stderr io.Writer) int {
	client, err := authenticatedClient(flags)
	if err != nil {
		return reportError(err, stderr)
	}
	if err := client.RemoveFavorite(modelID); err != nil {
		return reportError(err, stderr)
	}
	if handled, code := emitJSON(stdout, stderr, flags.jsonOutput, map[string]interface{}{"ok": true, "model_id": modelID, "starred": false}); handled {
		return code
	}
	fmt.Fprintf(stdout, "%q unstarred\n", modelID)
	return ExitOK
}
