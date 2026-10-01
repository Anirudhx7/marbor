package router

import "sort"

// SetModelAliases replaces the alias map (client-facing name -> real model).
// The input is copied, and the copy is published with a single atomic swap,
// so a concurrent ResolveModelAlias always sees either the complete old map
// or the complete new one and the per-request path never takes a lock. The
// caller keeps ownership of aliases and may reuse or mutate it afterwards.
// Callers are expected to pass an already-validated map (one hop, no empty
// names); nil or empty clears every alias.
func (r *Router) SetModelAliases(aliases map[string]string) {
	if len(aliases) == 0 {
		r.modelAliases.Store(nil)
		return
	}
	cp := make(map[string]string, len(aliases))
	for k, v := range aliases {
		cp[k] = v
	}
	r.modelAliases.Store(&cp)
}

// ResolveModelAlias returns the real model an alias points to, and whether
// name is an alias at all. Resolution is a single hop by construction:
// validation guarantees no target is itself an alias.
func (r *Router) ResolveModelAlias(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	m := r.modelAliases.Load()
	if m == nil {
		return "", false
	}
	target, ok := (*m)[name]
	return target, ok
}

// ModelAliases returns a fresh copy of the current alias map (never nil), so
// callers can range or modify it without touching the published map.
func (r *Router) ModelAliases() map[string]string {
	out := make(map[string]string)
	if m := r.modelAliases.Load(); m != nil {
		for k, v := range *m {
			out[k] = v
		}
	}
	return out
}

// Status values reported by FleetModelStatuses.
const (
	FleetModelLoaded    = "loaded"
	FleetModelAvailable = "available"
)

// FleetModelStatuses returns every model name currently present on a healthy
// node, mapped to "loaded" (resident in memory, from the live poll) or
// "available" (downloaded, from the node's cached /api/tags catalog). Loaded
// wins over available. A model on no healthy node, or on a node whose catalog
// could not be fetched, is simply absent: callers must treat absence as
// "not known to be present", never as present.
func (r *Router) FleetModelStatuses() map[string]string {
	seen, _ := r.FleetModelInventory()
	return seen
}

// FleetModelInventory is FleetModelStatuses plus whether the inventory is
// complete: true only when at least one node is healthy and every healthy
// node's catalog was fetched. When it is false, a name's absence from the
// map means "not checked", not "not on the fleet".
func (r *Router) FleetModelInventory() (map[string]string, bool) {
	seen := make(map[string]string)
	healthyNodes := 0
	complete := true
	for _, n := range r.Nodes() {
		n.RLock()
		healthy := n.Healthy
		loaded := n.LoadedModels
		nodeURL := n.URL
		n.RUnlock()
		if !healthy {
			continue
		}
		healthyNodes++
		for _, m := range loaded {
			seen[m.Name] = FleetModelLoaded
		}
		tags, err := r.FetchModelTags(nodeURL)
		if err != nil {
			complete = false
			continue
		}
		for _, t := range tags {
			if _, exists := seen[t.Name]; !exists {
				seen[t.Name] = FleetModelAvailable
			}
		}
	}
	return seen, complete && healthyNodes > 0
}

// SortedModelAliasNames returns the alias names in a stable order.
func SortedModelAliasNames(aliases map[string]string) []string {
	names := make([]string, 0, len(aliases))
	for k := range aliases {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
