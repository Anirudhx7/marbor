package router

import (
	"log"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// warmStateFlushInterval is the Tier-2 background cadence: the full in-memory
// residency snapshot is flushed to SQLite this often, catching drift (VRAM,
// last-used) that the immediate lifecycle writes don't cover.
const warmStateFlushInterval = 60 * time.Second

// SetStore attaches the persistence store used for the warm-state residency map.
// Call once at startup before Start(); a nil store disables all warm-state
// persistence (the default for a Router built directly in tests).
func (r *Router) SetStore(s store.Store) {
	r.mu.Lock()
	r.store = s
	r.mu.Unlock()
}

// warmStore returns the configured store, or nil. Reads under r.mu so it never
// races with SetStore. Callers that already hold r.mu must read r.store directly.
func (r *Router) warmStore() store.Store {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store
}

// persistResidencyDiff records a model-residency change to the store the moment
// the mesh observes it (Tier 1, lifecycle events): models newly resident on the
// node are recorded as loads (bumping load_count); models that vanished are
// deleted. Persistence is best-effort — a store error must never disturb polling
// — so failures are logged and swallowed. Must be called WITHOUT holding n.mu or
// r.mu (it performs blocking store I/O).
func (r *Router) persistResidencyDiff(node string, prev, current []ModelInfo) {
	st := r.warmStore()
	if st == nil {
		return
	}
	prevSet := make(map[string]struct{}, len(prev))
	for _, m := range prev {
		prevSet[m.Name] = struct{}{}
	}
	curSet := make(map[string]struct{}, len(current))
	for _, m := range current {
		curSet[m.Name] = struct{}{}
		if _, existed := prevSet[m.Name]; existed {
			continue // unchanged residency — refreshed by the Tier-2 snapshot
		}
		if err := st.RecordWarmLoad(store.WarmStateRecord{
			Model:     m.Name,
			Node:      node,
			LastUsed:  r.lastUsedAt(node, m.Name),
			VRAMBytes: m.SizeVRAM,
		}); err != nil {
			log.Printf("warmstate: record load %q on %s: %v", m.Name, node, err)
		}
	}
	for _, m := range prev {
		if _, still := curSet[m.Name]; still {
			continue
		}
		if err := st.DeleteWarmState(m.Name, node); err != nil {
			log.Printf("warmstate: delete %q on %s: %v", m.Name, node, err)
		}
	}
}

// reconcileNodeResidency calls ReconcileNodeWarmState so that after the first
// successful /api/ps poll for a node, every warm_state row for that node
// exactly matches what Ollama says is resident. Stale rows left by a previous
// process run (or by a restore that ran after a poll that saw nothing) are
// pruned here deterministically, regardless of restore/poll ordering.
//
// Must be called WITHOUT holding n.mu or r.mu (it performs blocking store I/O).
func (r *Router) reconcileNodeResidency(node string, current []ModelInfo) {
	st := r.warmStore()
	if st == nil {
		return
	}
	names := make([]string, len(current))
	for i, m := range current {
		names[i] = m.Name
	}
	if err := st.ReconcileNodeWarmState(node, names); err != nil {
		log.Printf("warmstate: reconcile %s: %v", node, err)
	}
}

// snapshotNode flushes one node's current residency snapshot immediately (Tier 1
// for the node-unhealthy transition: capture the last-known warm set before the
// node potentially goes away). Best-effort; must be called without holding r.mu.
func (r *Router) snapshotNode(n *NodeState) {
	st := r.warmStore()
	if st == nil {
		return
	}
	n.mu.RLock()
	name := n.Name
	models := append([]ModelInfo(nil), n.LoadedModels...)
	n.mu.RUnlock()
	for _, m := range models {
		if err := st.SnapshotWarmState(store.WarmStateRecord{
			Model:     m.Name,
			Node:      name,
			LastUsed:  r.lastUsedAt(name, m.Name),
			VRAMBytes: m.SizeVRAM,
		}); err != nil {
			log.Printf("warmstate: snapshot %q on %s: %v", m.Name, name, err)
			return
		}
	}
}

// FlushWarmState writes the full in-memory residency snapshot to the store
// (Tier 2 background flush and Tier 3 shutdown flush). It refreshes last-used and
// VRAM for every resident (model, node) pair without disturbing load_count. It
// never deletes rows — models leaving VRAM are pruned by the immediate lifecycle
// writes (unload/eviction/poll-diff). Safe to call concurrently; best-effort.
func (r *Router) FlushWarmState() {
	r.mu.RLock()
	st := r.store
	nodes := append([]*NodeState(nil), r.nodes...)
	r.mu.RUnlock()
	if st == nil {
		return
	}
	for _, n := range nodes {
		n.mu.RLock()
		name := n.Name
		models := append([]ModelInfo(nil), n.LoadedModels...)
		n.mu.RUnlock()
		for _, m := range models {
			if err := st.SnapshotWarmState(store.WarmStateRecord{
				Model:     m.Name,
				Node:      name,
				LastUsed:  r.lastUsedAt(name, m.Name),
				VRAMBytes: m.SizeVRAM,
			}); err != nil {
				log.Printf("warmstate: flush %q on %s: %v", m.Name, name, err)
			}
		}
	}
}

// RestoreWarmState seeds the in-memory warm state from the store at startup so
// the router starts warm, not cold. It restores the LRU last-used history for
// every persisted (model, node) pair, and seeds each known node's residency map
// when it is still empty (i.e. the first health poll has not yet populated it —
// polling remains the authoritative source and overwrites the seed as soon as it
// succeeds). Returns the number of persisted pairs restored. Call after nodes are
// registered and before serving client traffic.
func (r *Router) RestoreWarmState() (int, error) {
	r.mu.RLock()
	st := r.store
	nodes := append([]*NodeState(nil), r.nodes...)
	r.mu.RUnlock()
	if st == nil {
		return 0, nil
	}
	rows, err := st.AllWarmState()
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	byNode := make(map[string][]ModelInfo)
	r.lruMu.Lock()
	if r.lastUsed == nil {
		r.lastUsed = make(map[string]time.Time)
	}
	for _, w := range rows {
		if !w.LastUsed.IsZero() {
			r.lastUsed[modelKey(w.Node, w.Model)] = w.LastUsed
		}
		byNode[w.Node] = append(byNode[w.Node], ModelInfo{Name: w.Model, SizeVRAM: w.VRAMBytes})
	}
	r.lruMu.Unlock()

	for _, n := range nodes {
		models, ok := byNode[n.Name]
		if !ok {
			continue
		}
		n.mu.Lock()
		// Only seed if a poll has not already established the real residency — the
		// live /api/ps result is always more authoritative than the restored guess.
		if len(n.LoadedModels) == 0 {
			n.LoadedModels = models
		}
		n.mu.Unlock()
	}
	return len(rows), nil
}
