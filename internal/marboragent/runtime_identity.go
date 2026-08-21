package marboragent

// runtime_identity.go gives each runtime DetectAll finds a stable RuntimeID,
// independent of its type name or port - both of those are attributes that
// can legitimately change (a runtime's listen port can be reconfigured),
// while the identity a mesh node row pins itself to must not. This mirrors
// identity.go's node_id persistence pattern (a JSON file next to it, same
// directory, same "best effort, never fail startup over it" posture) one
// layer down, for the runtimes running *on* this host rather than the host
// itself. See runtime_detect.go for DetectedRuntime.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// runtimesFileName is the local state file the agent persists its runtime
// identity registry in, alongside node_id (see nodeIDDir).
const runtimesFileName = "runtimes.json"

// runtimeRegistryEntry is one persisted runtime identity. LastName/LastPort
// are matching heuristics only, refreshed every reconciliation - never the
// identity itself (RuntimeID is).
type runtimeRegistryEntry struct {
	RuntimeID string    `json:"runtime_id"`
	LastName  string    `json:"last_name"`
	LastPort  int       `json:"last_port"`
	LastSeen  time.Time `json:"last_seen"`
}

// runtimeRegistry holds this install's persisted runtime identities and
// reconciles each detection cycle's results against them. Not safe for
// concurrent use - Scheduler only ever calls Reconcile from its single
// refresh goroutine (Seed, then the Start ticker), never concurrently with
// itself.
type runtimeRegistry struct {
	path    string
	entries []runtimeRegistryEntry
}

// loadOrCreateRuntimeRegistry loads the persisted registry, or starts an
// empty one if the file is missing/corrupt - same "never fail startup over
// this" posture as loadOrCreateNodeID. An empty registry just means every
// runtime detected this cycle mints a fresh RuntimeID, which is exactly
// correct behavior for a brand-new install.
func loadOrCreateRuntimeRegistry() *runtimeRegistry {
	path := filepath.Join(nodeIDDir(), runtimesFileName)
	reg := &runtimeRegistry{path: path}
	if b, err := os.ReadFile(path); err == nil {
		var entries []runtimeRegistryEntry
		if json.Unmarshal(b, &entries) == nil {
			reg.entries = entries
		}
	}
	return reg
}

// Reconcile assigns each detected runtime a stable RuntimeID, in the same
// order as detected, and persists the updated registry. Matching rules,
// applied in order (type and port are heuristics used only to find the
// right existing identity, never surfaced as the identity itself):
//  1. Exact port match against a registry entry not already claimed this
//     cycle -> reuse that RuntimeID (handles a runtime restarting/
//     upgrading in place on the same port).
//  2. No port match, but exactly one not-yet-claimed registry entry has the
//     same LastName and no other detected runtime this cycle already
//     matched it by port -> reuse that RuntimeID (handles a single-
//     instance-per-type runtime whose port was reconfigured - the specific
//     "8000 -> 8001 must not create a new identity" case).
//  3. Otherwise -> mint a new RuntimeID.
//
// Registry entries not matched this cycle are kept (stale, not deleted) so
// one transient probe miss doesn't churn identity on the next successful
// cycle.
func (r *runtimeRegistry) Reconcile(detected []DetectedRuntime) []RuntimeInfo {
	now := time.Now().UTC()
	claimed := make([]bool, len(r.entries))
	out := make([]RuntimeInfo, len(detected))

	assign := func(outIdx, entryIdx int, d DetectedRuntime) {
		claimed[entryIdx] = true
		r.entries[entryIdx].LastName = d.Name
		r.entries[entryIdx].LastPort = d.Port
		r.entries[entryIdx].LastSeen = now
		out[outIdx] = RuntimeInfo{Name: d.Name, ID: r.entries[entryIdx].RuntimeID, Port: d.Port}
	}

	// Pass 1: exact port match.
	for i, d := range detected {
		for j, e := range r.entries {
			if claimed[j] || e.LastPort != d.Port {
				continue
			}
			assign(i, j, d)
			break
		}
	}

	// Pass 2: single-candidate type match for anything pass 1 left
	// unresolved (out[i].ID == ""). Only applies when exactly one
	// not-yet-claimed registry entry shares the type name - if two or more
	// candidates share it, the heuristic can't disambiguate, so it falls
	// through to minting a fresh identity for each (documented limitation
	// for simultaneous same-type port moves - see plan §1).
	for i, d := range detected {
		if out[i].ID != "" {
			continue
		}
		candidateIdx, count := -1, 0
		for j, e := range r.entries {
			if claimed[j] || e.LastName != d.Name {
				continue
			}
			count++
			candidateIdx = j
		}
		if count == 1 {
			assign(i, candidateIdx, d)
		}
	}

	// Pass 3: mint a fresh identity for anything still unresolved.
	for i, d := range detected {
		if out[i].ID != "" {
			continue
		}
		id := newUUIDv4()
		r.entries = append(r.entries, runtimeRegistryEntry{
			RuntimeID: id,
			LastName:  d.Name,
			LastPort:  d.Port,
			LastSeen:  now,
		})
		out[i] = RuntimeInfo{Name: d.Name, ID: id, Port: d.Port}
	}

	r.persist()
	return out
}

// persist is best-effort, same as loadOrCreateNodeID's write - a failure to
// save (read-only filesystem, permissions) means the next cycle may re-mint
// identities it otherwise would have reused, which degrades gracefully
// rather than failing telemetry collection.
func (r *runtimeRegistry) persist() {
	b, err := json.Marshal(r.entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(r.path, b, 0o600)
}
