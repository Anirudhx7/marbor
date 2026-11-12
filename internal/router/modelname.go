package router

import "strings"

// ModelNamesEquivalent reports whether a and b name the same model under
// Ollama's bare-name shorthand, where "llama3" means "llama3:latest". The
// runtime's own listings (/api/tags, /api/ps) always return the tagged form,
// so an operator-typed bare name must still match its ":latest" entry.
//
// Exact semantics, nothing broader:
//   - identical strings are equivalent;
//   - a digest-pinned reference (containing "@") only matches exactly;
//   - a name is tagged when it has a ":" after its last "/", so a registry
//     port ("host:5000/ns/model") does not count as a tag;
//   - when exactly one side is untagged, they are equivalent only if the
//     tagged side is the untagged side plus ":latest";
//   - anything else (two different tags, two different bare names) is not.
//
// Case-sensitive, no whitespace trimming, no other normalization.
func ModelNamesEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	if strings.Contains(a, "@") || strings.Contains(b, "@") {
		return false
	}
	aTagged, bTagged := modelNameTagged(a), modelNameTagged(b)
	switch {
	case aTagged && !bTagged:
		return a == b+":latest"
	case !aTagged && bTagged:
		return b == a+":latest"
	default:
		return false
	}
}

// modelNameTagged reports whether name carries an explicit tag, i.e. a ":"
// that appears after the last "/" (so a registry host:port is not a tag).
func modelNameTagged(name string) bool {
	return strings.LastIndex(name, ":") > strings.LastIndex(name, "/")
}

// findLoadedModel returns the resident entry for name from models: an exact
// name match first, otherwise the first entry ModelNamesEquivalent to name.
// The returned entry's Name is the runtime-reported form, which is the key
// the digest reference was recorded under.
func findLoadedModel(models []ModelInfo, name string) (ModelInfo, bool) {
	for _, m := range models {
		if m.Name == name {
			return m, true
		}
	}
	for _, m := range models {
		if ModelNamesEquivalent(name, m.Name) {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// WarmupSupportedForRuntime reports whether keep-warm and scheduled warmup
// can act on a node running runtime. The warmup mechanism is Ollama's
// /api/generate keep_alive, so only Ollama (or a node whose runtime is not
// yet known, which is treated as Ollama everywhere else) qualifies. The one
// shared rule used by the warmer, the scheduler, the predictive engine and
// the admin API's warmupSupported field.
func WarmupSupportedForRuntime(runtime string) bool {
	return runtime == "ollama" || runtime == ""
}

// driftedResidentDigest reports whether model is resident on n under a digest
// that differs from the fleet's reference digest for that model. The lookup
// uses the resident entry's runtime-reported name (the key the reference was
// recorded under), not the configured name, so a bare-named config is
// checked against its ":latest" reference. Not resident, or no digest on
// either side, is never flagged.
func (r *Router) driftedResidentDigest(n *NodeState, model string) bool {
	n.mu.RLock()
	entry, ok := findLoadedModel(n.LoadedModels, model)
	n.mu.RUnlock()
	if !ok {
		return false
	}
	return r.digestMismatch(entry.Name, entry.Digest)
}

// ModelNamesMatchOnRuntime reports whether a and b name the same model on a
// node running runtime. The bare-name ":latest" shorthand is an Ollama
// convention, so equivalence applies only to Ollama (or an unset runtime,
// which routing treats as Ollama); every other runtime serves exact ids and
// matches exactly.
func ModelNamesMatchOnRuntime(runtime, a, b string) bool {
	if a == b {
		return true
	}
	return normalizeRuntime(runtime) == runtimeOllama && ModelNamesEquivalent(a, b)
}

// reservationModelKey returns the key a warm reservation for model on node is
// stored under. On an Ollama node (or one with an unset runtime) the bare name
// and its ":latest" form share one key, so a reservation made under one is seen
// and cleared under the other. On any other runtime, or a node the router does
// not know, the name is used exactly; the unknown-node case is deliberate, as
// there is no runtime to judge by. It takes the router and node locks, so
// callers must not hold r.mu or n.mu, and must resolve it before taking
// evictMu. The key is derived from the runtime at call time: if the node's
// runtime changes between a reserve and its clear, the old key stays until the
// reservation TTL expires it.
func (r *Router) reservationModelKey(node, model string) string {
	n := r.FindNode(node)
	if n == nil || normalizeRuntime(n.GetRuntime()) != runtimeOllama {
		return model
	}
	return canonicalModelName(model)
}
