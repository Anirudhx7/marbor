package router

// SetFallbackChains replaces the fallback chain map (model -> ordered
// alternates tried when the model does not fit anywhere). The input is deep
// copied, and the copy is published with a single atomic swap, so a concurrent
// FallbackChainFor always sees either the complete old map or the complete new
// one and the per-request path takes no lock. The caller keeps ownership of
// chains. Nil or empty clears every chain.
func (r *Router) SetFallbackChains(chains map[string][]string) {
	r.fallbackChains.Store(copyChains(chains))
}

// SetLocalDegradationChains replaces the local degradation chain map (model ->
// ordered local alternates tried when no node can serve it). Same copy and
// atomic-swap behavior as SetFallbackChains.
func (r *Router) SetLocalDegradationChains(chains map[string][]string) {
	r.localDegradationChains.Store(copyChains(chains))
}

// copyChains deep copies a chain map into fresh backing arrays and returns a
// pointer suitable for an atomic store, or nil for a nil or empty map.
func copyChains(chains map[string][]string) *map[string][]string {
	if len(chains) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(chains))
	for model, alts := range chains {
		cp[model] = append([]string(nil), alts...)
	}
	return &cp
}
