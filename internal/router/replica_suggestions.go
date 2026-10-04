package router

// replica_suggestions.go - turns the multi-host launch evidence agents report
// into suggested replica declarations an operator can confirm.
//
// Evidence never feeds the scheduler. correlate() is a pure function from node
// rows plus host snapshots to suggestions; a suggestion only changes anything
// after an operator confirms it, and confirming writes ordinary declared
// replica_peers on every member (ApplyReplicaPeersBatch). The resolver that
// turns declarations into head and worker roles is untouched, so there is still
// exactly one state machine.
//
// The correlator is deliberately conservative: it never infers a missing rank,
// never guesses which host an ambiguous address means, never resolves DNS, and
// shows every doubtful case with its reason instead of a confirmable group.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/store"
)

// Suggestion states.
const (
	SuggestionComplete            = "complete"
	SuggestionIncomplete          = "incomplete"
	SuggestionConflicting         = "conflicting"
	SuggestionContradictsDeclared = "contradicts_declared"
)

// Launchers a suggestion can come from.
const (
	LauncherVLLMMultiprocessing = "vllm-mp"
	LauncherLlamaCPPRPC         = "llamacpp-rpc"
)

// Coverage states, one per node row.
const (
	CoverageReporting         = "reporting"
	CoverageAgentUpdateNeeded = "agent_update_needed"
	CoverageNoAgent           = "no_agent"
	CoverageEnvUnreadable     = "env_unreadable"
)

// capTopology is the agent capability that says launch details are reported.
const capTopology = "deployment.topology"

// SuggestionEvidence says what one node's agent reported that led to a
// suggestion.
type SuggestionEvidence struct {
	Node   string
	Source string
	Detail string
}

// DeclaredReplica is one member's own declared replica_peers, shown next to a
// suggestion that disagrees with it.
type DeclaredReplica struct {
	Node    string
	Members []string
	Head    string
}

// ReplicaSuggestion is one detected multi-host group.
type ReplicaSuggestion struct {
	Fingerprint string
	Launcher    string
	Runtime     string
	State       string
	// Reason is one plain sentence for any state other than complete.
	Reason  string
	Head    string
	Members []string
	// Evidence lists what each node's agent reported.
	Evidence []SuggestionEvidence
	// Missing lists what is not yet known: absent ranks, unregistered hosts.
	Missing []string
	// Declared lists members that already carry a declaration.
	Declared []DeclaredReplica
	// Confirmable is true only for a complete suggestion.
	Confirmable bool
}

// TopologyCoverage says, per node row, whether multi-host detection can see it.
type TopologyCoverage struct {
	Node  string
	Host  string
	State string
	// Detected is true when a multi-host launch was found on this row.
	Detected bool
	Detail   string
}

// nodeView is the plain copy of the NodeState fields correlation needs, read
// under each node's lock so the correlator itself touches no locks.
type nodeView struct {
	Name     string
	Host     string
	Port     int
	Runtime  string
	PinnedID string
	Declared *store.ReplicaPeers
}

// ReplicaSuggestions returns the current fleet-wide suggestions and per-node
// detection coverage. It snapshots node rows and host evidence separately and
// never holds the evidence lock together with a node or router lock.
func (r *Router) ReplicaSuggestions() ([]ReplicaSuggestion, []TopologyCoverage) {
	r.mu.RLock()
	nodes := make([]*NodeState, len(r.nodes))
	copy(nodes, r.nodes)
	r.mu.RUnlock()

	views := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		n.mu.RLock()
		v := nodeView{
			Name:     n.Name,
			Host:     n.Host,
			Port:     portOf(n.URL),
			Runtime:  n.Runtime,
			PinnedID: n.AgentRuntimeID,
		}
		if n.ReplicaPeers != nil {
			rp := store.ReplicaPeers{Head: n.ReplicaPeers.Head, Members: append([]string(nil), n.ReplicaPeers.Members...)}
			v.Declared = &rp
		}
		n.mu.RUnlock()
		views = append(views, v)
	}
	return correlate(views, r.snapshotHostEvidence())
}

// ErrReplicaMemberMissing is returned by ApplyReplicaPeersBatch when a named
// node is no longer registered.
var ErrReplicaMemberMissing = errors.New("replica member is no longer registered")

// ApplyReplicaPeersBatch declares replica membership on every node named in
// assign, all or nothing. While holding the router's read lock (so a node
// cannot be removed or replaced underneath it) it checks that every named node
// exists, calls persist so the store is written first, and only then sets the
// declarations in memory. Nothing can fail after persist succeeds, so memory
// and store never diverge. persist may be nil; it must not take the router lock.
//
// Node locks are taken in sorted-name order, the only place more than one node
// lock is held at once. A concurrent resolver reads nodes one at a time, so it
// can briefly see a mix of old and new declarations; the resolver fails closed
// to unresolved on any mismatch, which is the safe direction.
//
// The read lock is deliberately held across persist: releasing it would let a
// node be removed between the existence check and the write. The admin node
// delete handler, node PATCH (including a URL change) and replica confirm all
// take the admin node-patch mutex first, so they never overlap an apply. A
// config reload (SyncNodes) does not take that mutex: if it removes or
// replaces a node it waits on the router lock until the apply finishes, and
// while it waits new routing readers wait behind it. The cost is bounded by one
// store transaction, which is bounded by the store's busy timeout. A caller
// outside the admin API must not rely on RemoveNode or UpdateNodeURL running
// concurrently with an apply.
func (r *Router) ApplyReplicaPeersBatch(assign map[string]store.ReplicaPeers, persist func(map[string]store.ReplicaPeers) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byName := make(map[string]*NodeState, len(assign))
	for _, n := range r.nodes {
		if _, want := assign[n.Name]; want {
			byName[n.Name] = n
		}
	}
	names := make([]string, 0, len(assign))
	for name := range assign {
		if byName[name] == nil {
			return fmt.Errorf("%w: %s", ErrReplicaMemberMissing, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	if persist != nil {
		if err := persist(assign); err != nil {
			return err
		}
	}
	for _, name := range names {
		rp := assign[name]
		cp := store.ReplicaPeers{Head: rp.Head, Members: append([]string(nil), rp.Members...)}
		n := byName[name]
		n.mu.Lock()
		n.ReplicaPeers = &cp
		n.mu.Unlock()
	}
	return nil
}

// ---- correlation ----

// attribution ties one deployment report to the single node row it belongs to.
type attribution struct {
	row  nodeView
	dep  marboragent.DeploymentReport
	topo *marboragent.Topology
	kind string // LauncherVLLMMultiprocessing or LauncherLlamaCPPRPC
}

// candidate is a detected group before declared-membership rules are applied.
type candidate struct {
	launcher string
	runtime  string
	state    string
	reason   string
	head     string
	members  []string
	evidence []SuggestionEvidence
	missing  []string
}

type correlator struct {
	rowsByHost map[string][]nodeView
	hostKeys   []string
	hosts      map[string]HostEvidence
	declared   map[string]*store.ReplicaPeers
	rows       map[string]nodeView
}

type resolveStatus int

const (
	resolveOK resolveStatus = iota
	resolveNone
	resolveAmbiguous
	resolveLoopback
)

// correlate builds suggestions and coverage. Pure: no locks, no clocks, no I/O,
// and independent of the order of nodes and of each host's deployments.
func correlate(nodes []nodeView, hosts map[string]HostEvidence) ([]ReplicaSuggestion, []TopologyCoverage) {
	c := &correlator{
		rowsByHost: make(map[string][]nodeView),
		hosts:      make(map[string]HostEvidence),
		declared:   make(map[string]*store.ReplicaPeers),
		rows:       make(map[string]nodeView),
	}
	for _, n := range nodes {
		c.rowsByHost[n.Host] = append(c.rowsByHost[n.Host], n)
		c.rows[n.Name] = n
		if n.Declared != nil && len(n.Declared.Members) > 0 {
			c.declared[n.Name] = n.Declared
		}
	}
	for h := range c.rowsByHost {
		sort.Slice(c.rowsByHost[h], func(i, j int) bool { return c.rowsByHost[h][i].Name < c.rowsByHost[h][j].Name })
		c.hostKeys = append(c.hostKeys, h)
	}
	sort.Strings(c.hostKeys)
	// Evidence for a host that has no node row cannot identify anything.
	for h, ev := range hosts {
		if _, ok := c.rowsByHost[h]; ok {
			c.hosts[h] = ev
		}
	}

	var cands []candidate
	attByRow := make(map[string][]attribution)
	envUnreadable := make(map[string]bool)

	for _, h := range c.hostKeys {
		ev, ok := c.hosts[h]
		if !ok {
			continue
		}
		for _, dep := range ev.Deployments {
			if dep.Topology == nil {
				continue
			}
			kind := topologyKind(dep)
			intent := hasDistributedIntent(kind, dep.Topology)
			unreadable := hasEnvUnreadable(dep.Topology)
			if !intent && !unreadable {
				continue
			}
			row, cands2, ok := c.attribute(h, dep)
			if !ok {
				// An ambiguous report of real distributed intent is shown;
				// an unreadable environment on an ambiguous host is not.
				if intent && len(cands2) > 0 {
					cands = append(cands, ambiguousAttribution(kind, cands2))
				}
				continue
			}
			if !intent {
				envUnreadable[row.Name] = true
				continue
			}
			if want := kindRuntime(kind); row.Runtime != "" && row.Runtime != "auto" && row.Runtime != want {
				cands = append(cands, candidate{
					launcher: kind, runtime: want, state: SuggestionConflicting, head: row.Name,
					members: []string{row.Name},
					reason:  fmt.Sprintf("agent reports a %s launch for node %s, which is registered as %s", want, row.Name, row.Runtime),
				})
				continue
			}
			attByRow[row.Name] = append(attByRow[row.Name], attribution{row: row, dep: dep, topo: dep.Topology, kind: kind})
		}
	}

	var vllm []attribution
	var rpc []attribution
	rowNames := make([]string, 0, len(attByRow))
	for name := range attByRow {
		rowNames = append(rowNames, name)
	}
	sort.Strings(rowNames)
	for _, name := range rowNames {
		atts := attByRow[name]
		if len(atts) > 1 {
			kind := atts[0].kind
			for _, a := range atts[1:] {
				if a.kind < kind {
					kind = a.kind // independent of the order the agent listed them
				}
			}
			cands = append(cands, candidate{
				launcher: kind, runtime: kindRuntime(kind), state: SuggestionConflicting, head: name,
				members: []string{name},
				reason:  fmt.Sprintf("more than one multi-host launch is attributed to node %s", name),
			})
			continue
		}
		if atts[0].kind == LauncherVLLMMultiprocessing {
			vllm = append(vllm, atts[0])
		} else {
			rpc = append(rpc, atts[0])
		}
	}
	cands = append(cands, c.vllmGroups(vllm)...)
	for _, a := range rpc {
		cands = append(cands, c.rpcGroup(a)...)
	}

	suggestions := make([]ReplicaSuggestion, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, cd := range cands {
		s, ok := c.finalize(cd)
		if !ok {
			continue
		}
		// Two reports that say exactly the same thing (for example two
		// unplaceable workers on one host) are one suggestion.
		key := s.Fingerprint + "|" + s.Launcher + "|" + s.State + "|" + s.Reason
		if seen[key] {
			continue
		}
		seen[key] = true
		suggestions = append(suggestions, s)
	}
	sort.Slice(suggestions, func(i, j int) bool {
		a, b := suggestions[i], suggestions[j]
		if a.Head != b.Head {
			return a.Head < b.Head
		}
		am, bm := strings.Join(a.Members, ","), strings.Join(b.Members, ",")
		if am != bm {
			return am < bm
		}
		if a.Launcher != b.Launcher {
			return a.Launcher < b.Launcher
		}
		return a.State < b.State
	})
	return suggestions, c.coverage(attByRow, envUnreadable)
}

// topologyKind names the launcher a deployment's topology evidence belongs to,
// from the launcher label first and the runtime second. "" means neither.
func topologyKind(dep marboragent.DeploymentReport) string {
	switch dep.Topology.Launcher {
	case LauncherVLLMMultiprocessing, LauncherLlamaCPPRPC:
		return dep.Topology.Launcher
	}
	switch dep.Runtime {
	case "vllm":
		return LauncherVLLMMultiprocessing
	case "llamacpp":
		return LauncherLlamaCPPRPC
	}
	return ""
}

func kindRuntime(kind string) string {
	if kind == LauncherVLLMMultiprocessing {
		return "vllm"
	}
	return "llamacpp"
}

// hasDistributedIntent is the first filter: only a launch that itself states
// multi-host intent can form a group.
func hasDistributedIntent(kind string, t *marboragent.Topology) bool {
	switch kind {
	case LauncherVLLMMultiprocessing:
		return (t.NNodes != nil && *t.NNodes > 1) || t.NodeRank != nil || t.MasterAddr != "" || t.RoleHint != ""
	case LauncherLlamaCPPRPC:
		return len(t.RPCServers) > 0
	}
	return false
}

func hasEnvUnreadable(t *marboragent.Topology) bool {
	for _, e := range t.Evidence {
		if strings.HasPrefix(e, "env-unreadable:") {
			return true
		}
	}
	return false
}

// attribute finds the one node row on host that a deployment report belongs
// to. A report with a port or runtime ID must match a row by it. A report with
// neither (a headless worker serves no port) belongs to a host's only row; with
// several rows it cannot be placed, and the candidates are returned.
func (c *correlator) attribute(host string, dep marboragent.DeploymentReport) (nodeView, []nodeView, bool) {
	rows := c.rowsByHost[host]
	if dep.Port > 0 || dep.RuntimeID != "" {
		var matches []nodeView
		for _, r := range rows {
			if (r.PinnedID != "" && r.PinnedID == dep.RuntimeID) || (dep.Port > 0 && r.Port == dep.Port) {
				matches = append(matches, r)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil, true
		}
		return nodeView{}, matches, false
	}
	if len(rows) == 1 {
		return rows[0], nil, true
	}
	return nodeView{}, rows, false
}

func ambiguousAttribution(kind string, rows []nodeView) candidate {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	sort.Strings(names)
	return candidate{
		launcher: kind, runtime: kindRuntime(kind), state: SuggestionConflicting,
		members: names,
		reason:  fmt.Sprintf("a multi-host launch was reported but cannot be matched to one of nodes %s on the same host", strings.Join(names, ", ")),
	}
}

// ---- address identity ----

func normalizeAddr(a string) string {
	a = strings.TrimSpace(a)
	a = strings.TrimPrefix(a, "[")
	a = strings.TrimSuffix(a, "]")
	a = strings.TrimSuffix(strings.ToLower(a), ".")
	if ip := net.ParseIP(a); ip != nil {
		return ip.String()
	}
	return a
}

func isLoopbackAddr(a string) bool {
	if a == "localhost" {
		return true
	}
	ip := net.ParseIP(a)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// resolve maps an address as typed in a launch command to the one host it
// names. Order: a node row's own host string, then an agent's hostname, then
// the addresses an agent reports for itself. An address that matches two hosts
// at any step is ambiguous (a container bridge address is typically on every
// host), and one that matches none stays unmatched. No DNS lookups.
func (c *correlator) resolve(addr string) (string, resolveStatus) {
	a := normalizeAddr(addr)
	if a == "" {
		return "", resolveNone
	}
	if isLoopbackAddr(a) {
		return "", resolveLoopback
	}
	if h, st, done := c.uniqueHost(func(h string) bool { return normalizeAddr(h) == a }, c.hostKeys); done {
		return h, st
	}
	var evHosts []string
	for _, h := range c.hostKeys {
		if _, ok := c.hosts[h]; ok {
			evHosts = append(evHosts, h)
		}
	}
	if h, st, done := c.uniqueHost(func(h string) bool { return normalizeAddr(c.hosts[h].Hostname) == a }, evHosts); done {
		return h, st
	}
	if h, st, done := c.uniqueHost(func(h string) bool {
		for _, x := range c.hosts[h].Addrs {
			if normalizeAddr(x) == a {
				return true
			}
		}
		return false
	}, evHosts); done {
		return h, st
	}
	return "", resolveNone
}

func (c *correlator) uniqueHost(match func(string) bool, candidates []string) (string, resolveStatus, bool) {
	var found []string
	for _, h := range candidates {
		if match(h) {
			found = append(found, h)
		}
	}
	switch len(found) {
	case 0:
		return "", resolveNone, false
	case 1:
		return found[0], resolveOK, true
	}
	return "", resolveAmbiguous, true
}

// ---- vLLM multiprocessing ----

// vllmGroups groups attributed vLLM reports that name the same master
// (resolved host plus port) and builds one candidate per group.
func (c *correlator) vllmGroups(atts []attribution) []candidate {
	var out []candidate
	groups := make(map[string][]attribution)
	masterHost := make(map[string]string)
	masterText := make(map[string]string)
	for _, a := range atts {
		t := a.topo
		if t.MasterAddr == "" {
			// A one node launch has no peers to match, so it is not a group.
			// Zero or negative cannot occur (the agent clamps the node count
			// to at least 1). An unknown (nil) count keeps the card.
			if t.NNodes != nil && *t.NNodes <= 1 {
				continue
			}
			out = append(out, candidate{
				launcher: a.kind, runtime: "vllm", state: SuggestionIncomplete, head: a.row.Name,
				members:  []string{a.row.Name},
				evidence: []SuggestionEvidence{vllmEvidence(a)},
				missing:  []string{fmt.Sprintf("node %s does not state a master address, so the other hosts cannot be matched to it", a.row.Name)},
				reason:   "the launch does not state a master address",
			})
			continue
		}
		port := 0
		if t.MasterPort != nil {
			port = *t.MasterPort
		}
		host, st := c.resolve(t.MasterAddr)
		var key string
		switch st {
		case resolveLoopback:
			if t.NNodes != nil && *t.NNodes > 1 {
				out = append(out, candidate{
					launcher: a.kind, runtime: "vllm", state: SuggestionConflicting, head: a.row.Name,
					members:  []string{a.row.Name},
					evidence: []SuggestionEvidence{vllmEvidence(a)},
					reason:   fmt.Sprintf("node %s uses a loopback master address (%s) for a %d-node launch, so it cannot be matched to other hosts", a.row.Name, t.MasterAddr, *t.NNodes),
				})
			}
			continue
		case resolveAmbiguous:
			key = "ambiguous:" + normalizeAddr(t.MasterAddr) + ":" + strconv.Itoa(port)
		case resolveNone:
			key = "addr:" + normalizeAddr(t.MasterAddr) + ":" + strconv.Itoa(port)
		default:
			key = "host:" + host + ":" + strconv.Itoa(port)
			masterHost[key] = host
		}
		masterText[key] = t.MasterAddr
		groups[key] = append(groups[key], a)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if cd, ok := c.vllmCandidate(k, groups[k], masterHost[k], masterText[k]); ok {
			out = append(out, cd)
		}
	}
	return out
}

func vllmEvidence(a attribution) SuggestionEvidence {
	t := a.topo
	var parts []string
	if t.NodeRank != nil {
		parts = append(parts, "rank "+strconv.Itoa(*t.NodeRank))
	}
	if t.NNodes != nil {
		parts = append(parts, "of "+strconv.Itoa(*t.NNodes)+" nodes")
	}
	if t.RoleHint != "" {
		parts = append(parts, t.RoleHint)
	}
	if t.MasterAddr != "" {
		m := t.MasterAddr
		if t.MasterPort != nil {
			m += ":" + strconv.Itoa(*t.MasterPort)
		}
		parts = append(parts, "master "+m)
	}
	return SuggestionEvidence{Node: a.row.Name, Source: evidenceSource(t), Detail: strings.Join(parts, ", ")}
}

func evidenceSource(t *marboragent.Topology) string {
	if len(t.Evidence) == 0 {
		return "agent report"
	}
	return strings.Join(t.Evidence, ", ")
}

func (c *correlator) vllmCandidate(key string, group []attribution, masterHost, masterText string) (candidate, bool) {
	sort.Slice(group, func(i, j int) bool { return group[i].row.Name < group[j].row.Name })
	cd := candidate{launcher: LauncherVLLMMultiprocessing, runtime: "vllm"}
	var conflicts, missing []string

	nnodes := make(map[int]bool)
	byRank := make(map[int][]string)
	for _, a := range group {
		cd.members = append(cd.members, a.row.Name)
		cd.evidence = append(cd.evidence, vllmEvidence(a))
		if a.topo.NNodes != nil {
			nnodes[*a.topo.NNodes] = true
		}
		if a.topo.NodeRank == nil {
			missing = append(missing, fmt.Sprintf("node %s does not state its rank", a.row.Name))
		} else {
			byRank[*a.topo.NodeRank] = append(byRank[*a.topo.NodeRank], a.row.Name)
		}
	}

	switch {
	case strings.HasPrefix(key, "ambiguous:"):
		conflicts = append(conflicts, fmt.Sprintf("master address %s matches more than one host", masterText))
	case strings.HasPrefix(key, "addr:"):
		missing = append(missing, fmt.Sprintf("master address %s does not match any registered node or agent address", masterText))
	}

	total := 0
	switch len(nnodes) {
	case 0:
		missing = append(missing, "the launch does not state how many nodes it spans")
	case 1:
		for n := range nnodes {
			total = n
		}
	default:
		var vs []string
		for n := range nnodes {
			vs = append(vs, strconv.Itoa(n))
		}
		sort.Strings(vs)
		conflicts = append(conflicts, "members disagree on the node count ("+strings.Join(vs, ", ")+")")
	}
	if len(nnodes) == 1 && total <= 1 {
		return candidate{}, false // a one-node launch is not a multi-host group
	}

	ranks := make([]int, 0, len(byRank))
	for r := range byRank {
		ranks = append(ranks, r)
	}
	sort.Ints(ranks)
	for _, r := range ranks {
		names := byRank[r]
		if len(names) > 1 {
			conflicts = append(conflicts, fmt.Sprintf("rank %d is claimed by %s", r, strings.Join(names, " and ")))
		}
		if total > 0 && (r < 0 || r >= total) {
			conflicts = append(conflicts, fmt.Sprintf("rank %d is outside 0 to %d", r, total-1))
		}
	}

	// The head is the rank 0 node on the host that owns the master address.
	if masterHost != "" {
		var onMaster []attribution
		for _, a := range group {
			if a.row.Host == masterHost {
				onMaster = append(onMaster, a)
			}
		}
		if len(onMaster) == 0 {
			missing = append(missing, fmt.Sprintf("the host that owns master address %s is not reporting a launch", masterText))
		}
		var heads []string
		for _, a := range onMaster {
			if a.topo.NodeRank != nil && *a.topo.NodeRank == 0 && a.topo.RoleHint != "worker" {
				heads = append(heads, a.row.Name)
				continue
			}
			conflicts = append(conflicts, fmt.Sprintf("node %s owns the master address but does not report rank 0 as a head", a.row.Name))
		}
		if len(heads) == 1 {
			cd.head = heads[0]
		}
	}

	if total > 1 {
		for r := 0; r < total; r++ {
			if len(byRank[r]) == 0 {
				missing = append(missing, fmt.Sprintf("rank %d has not been seen. Register its host as a node and update its agent; the worker will show down because a headless worker serves no HTTP", r))
			}
		}
	}

	for _, a := range group {
		if ev, ok := c.hosts[a.row.Host]; !ok || !hasCapability(ev.Capabilities, capTopology) {
			missing = append(missing, fmt.Sprintf("the agent for node %s is not reporting launch details", a.row.Name))
		}
	}

	cd.missing = dedupeSorted(missing)
	switch {
	case len(conflicts) > 0:
		cd.state = SuggestionConflicting
		cd.reason = strings.Join(dedupeSorted(conflicts), "; ")
	case len(cd.missing) > 0:
		cd.state = SuggestionIncomplete
		cd.reason = strings.Join(cd.missing, "; ")
	case cd.head == "":
		cd.state = SuggestionIncomplete
		cd.reason = "no head (rank 0 on the master host) was found"
		cd.missing = []string{cd.reason}
	default:
		cd.state = SuggestionComplete
	}
	return cd, true
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---- llama.cpp RPC ----

// rpcGroup builds the candidate for one llama.cpp head: the head plus the node
// row that owns each RPC server it lists. A worker host needs a node row but
// not a reporting agent, since an RPC server exposes no inference API.
func (c *correlator) rpcGroup(a attribution) []candidate {
	head := a.row.Name
	cd := candidate{launcher: a.kind, runtime: "llamacpp", head: head}
	servers := a.topo.RPCServers
	cd.evidence = append(cd.evidence, SuggestionEvidence{
		Node: head, Source: evidenceSource(a.topo), Detail: "RPC servers: " + strings.Join(servers, ", "),
	})

	members := map[string]bool{head: true}
	var conflicts, missing []string
	unregistered, loopback := 0, 0
	for _, s := range servers {
		host := s
		if h, _, err := net.SplitHostPort(s); err == nil {
			host = h
		}
		h, st := c.resolve(host)
		switch st {
		case resolveLoopback:
			loopback++
		case resolveNone:
			unregistered++
		case resolveAmbiguous:
			conflicts = append(conflicts, fmt.Sprintf("RPC server %s matches more than one host", s))
		default:
			rows := c.rowsByHost[h]
			if len(rows) != 1 {
				conflicts = append(conflicts, fmt.Sprintf("RPC server %s is on a host with %d node rows, so which node it is cannot be told", s, len(rows)))
				continue
			}
			w := rows[0].Name
			switch {
			case w == head:
				missing = append(missing, fmt.Sprintf("the head %s lists itself as an RPC server (%s)", head, s))
			case members[w]:
				missing = append(missing, fmt.Sprintf("two RPC servers resolve to the same node %s", w))
			default:
				members[w] = true
				cd.evidence = append(cd.evidence, SuggestionEvidence{Node: w, Source: "address match", Detail: "owns RPC server " + s})
			}
		}
	}
	if loopback == len(servers) {
		return nil // RPC servers on the head's own machine: nothing multi-host to suggest
	}
	if loopback > 0 {
		conflicts = append(conflicts, "the RPC server list mixes loopback and network addresses")
	}
	if unregistered > 0 {
		missing = append(missing, fmt.Sprintf("%d RPC servers not registered as nodes", unregistered))
	}
	for m := range members {
		cd.members = append(cd.members, m)
	}
	sort.Strings(cd.members)
	cd.missing = dedupeSorted(missing)
	switch {
	case len(conflicts) > 0:
		cd.state = SuggestionConflicting
		cd.reason = strings.Join(dedupeSorted(conflicts), "; ")
	case len(cd.missing) > 0:
		cd.state = SuggestionIncomplete
		cd.reason = strings.Join(cd.missing, "; ")
	default:
		cd.state = SuggestionComplete
	}
	return []candidate{cd}
}

// ---- declared membership and fingerprint ----

// finalize applies the declared-wins rules and computes the fingerprint. It
// reports false when the group is already declared identically on every member.
func (c *correlator) finalize(cd candidate) (ReplicaSuggestion, bool) {
	members := dedupeSorted(cd.members)
	s := ReplicaSuggestion{
		Fingerprint: fingerprint(cd.head, members),
		Launcher:    cd.launcher,
		Runtime:     cd.runtime,
		State:       cd.state,
		Reason:      cd.reason,
		Head:        cd.head,
		Members:     members,
		Evidence:    cd.evidence,
		Missing:     cd.missing,
	}
	identical := 0
	differing := false
	for _, m := range members {
		d, ok := c.declared[m]
		if !ok {
			continue
		}
		s.Declared = append(s.Declared, DeclaredReplica{Node: m, Members: dedupeSorted(d.Members), Head: d.Head})
		if d.Head == cd.head && equalStrings(dedupeSorted(d.Members), members) {
			identical++
		} else {
			differing = true
		}
	}
	if cd.state == SuggestionComplete {
		if identical == len(members) {
			return ReplicaSuggestion{}, false
		}
		if differing {
			s.State = SuggestionContradictsDeclared
			s.Reason = "a member already declares a different replica membership; clear or fix it first"
		}
	}
	s.Confirmable = s.State == SuggestionComplete
	return s, true
}

// fingerprint is the first 16 hex characters of sha256 over the head and the
// sorted members only, so a dismissal holds until the group's membership or
// head changes and not for any other change in its state.
func fingerprint(head string, sortedMembers []string) string {
	h := sha256.New()
	h.Write([]byte(head))
	for _, m := range sortedMembers {
		h.Write([]byte{0})
		h.Write([]byte(m))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- coverage ----

func (c *correlator) coverage(attByRow map[string][]attribution, envUnreadable map[string]bool) []TopologyCoverage {
	names := make([]string, 0, len(c.rows))
	for n := range c.rows {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]TopologyCoverage, 0, len(names))
	for _, name := range names {
		row := c.rows[name]
		cov := TopologyCoverage{Node: name, Host: row.Host}
		ev, ok := c.hosts[row.Host]
		switch {
		case !ok:
			cov.State = CoverageNoAgent
			cov.Detail = "no agent report for this host"
		case !hasCapability(ev.Capabilities, capTopology):
			cov.State = CoverageAgentUpdateNeeded
			cov.Detail = "agent update needed to detect multi-host launches"
		case len(attByRow[name]) > 0:
			cov.State = CoverageReporting
			cov.Detected = true
			cov.Detail = "multi-host launch detected"
		case envUnreadable[name]:
			cov.State = CoverageEnvUnreadable
			cov.Detail = "the launch environment could not be read, so RPC servers are unknown"
		default:
			cov.State = CoverageReporting
			cov.Detail = "nothing detected"
		}
		out = append(out, cov)
	}
	return out
}
