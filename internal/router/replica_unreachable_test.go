package router

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
)

// replicaStaleRouter builds a router with the given nodes, all healthy, and
// declares head/worker (plus any extra members) as one confirmed replica
// headed by "head". Every other node stays standalone.
func replicaStaleRouter(t *testing.T, cfgs []config.NodeConfig, members ...string) *Router {
	t.Helper()
	r := New(config.RoutingConfig{SessionAffinity: true}, cfgs, nil)
	decl := peers("head", members...)
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		for _, m := range members {
			if n.Name == m {
				n.ReplicaPeers = decl
			}
		}
		n.mu.Unlock()
	}
	return r
}

func nodeByName(t *testing.T, r *Router, name string) *NodeState {
	t.Helper()
	for _, n := range r.nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q not found", name)
	return nil
}

func setStaleByName(t *testing.T, r *Router, name string, stale bool) {
	t.Helper()
	r.setAgentStale(nodeByName(t, r, name), stale)
}

func excludedReasons(excluded []ExcludedCandidate) map[string]string {
	out := map[string]string{}
	for _, e := range excluded {
		out[e.Node] = e.Reason
	}
	return out
}

func twoNodeCfgs() []config.NodeConfig {
	return []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Runtime: "vllm", Host: "head-host"},
		{Name: "worker", URL: "http://worker.invalid:8000", Runtime: "vllm", Host: "worker-host"},
		{Name: "std", URL: "http://std.invalid:8000", Runtime: "vllm", Host: "std-host"},
	}
}

func TestHeadExcludedWhenWorkerAgentStale(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	setStaleByName(t, r, "worker", true)

	healthy, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Fatalf("head exclude reason = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}
	for _, n := range healthy {
		if n.Name == "head" {
			t.Fatal("head must not be a candidate while its worker's agent is stale")
		}
	}
	picked, _, dec := r.Route("", "", "")
	if picked == nil || picked.Name != "std" {
		t.Fatalf("Route picked %v, want the standalone node", picked)
	}
	if got := excludedReasons(dec.Excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("explain reason for head = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}
}

func TestHeadKeptWhenWorkerAgentless(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	// Agentless worker: AgentStale stays false, no matter what else is unknown.
	healthy, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if _, ok := excludedReasons(excluded)["head"]; ok {
		t.Fatal("head excluded although its worker has no agent (unknown must fail open)")
	}
	found := false
	for _, n := range healthy {
		found = found || n.Name == "head"
	}
	if !found {
		t.Fatal("head should stay a candidate")
	}
}

func TestHeadKeptWhenHeadlessWorkerUnhealthyButAgentPresent(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	w := nodeByName(t, r, "worker")
	w.mu.Lock()
	w.Healthy = false // headless workers never pass HTTP probing
	w.AgentPresent = true
	w.mu.Unlock()
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got, ok := excludedReasons(excluded)["head"]; ok {
		t.Fatalf("head excluded with reason %q although the worker's agent answers", got)
	}
}

func TestHeadUnreachableExistingReasonsWinPrecedence(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	setStaleByName(t, r, "worker", true)
	h := nodeByName(t, r, "head")
	h.mu.Lock()
	h.Healthy = false
	h.mu.Unlock()
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonUnhealthy {
		t.Errorf("head reason = %q, want %q (existing check must win)", got, ExcludeReasonUnhealthy)
	}
}

func TestHeadExcludedThreeNodeGroupOneStaleMember(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "w1", URL: "http://w1.invalid:8000", Host: "h1"},
		{Name: "w2", URL: "http://w2.invalid:8000", Host: "h2"},
		{Name: "std", URL: "http://std.invalid:8000", Host: "h3"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "w1", "w2")
	setStaleByName(t, r, "w2", true)
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("head reason = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}
}

func TestHeadExcludedForNonVLLMManualDeclaration(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8080", Runtime: "tgi", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8080", Runtime: "tgi", Host: "h1"},
		{Name: "std", URL: "http://std.invalid:8080", Runtime: "tgi", Host: "h2"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	setStaleByName(t, r, "worker", true)
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("tgi head reason = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}
}

func TestHeadCoHostedWithWorkerBothStale(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "shared"},
		{Name: "worker", URL: "http://worker.invalid:8001", Host: "shared"},
		{Name: "std", URL: "http://std.invalid:8000", Host: "other"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	setStaleByName(t, r, "head", true)
	setStaleByName(t, r, "worker", true)
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("co-hosted head reason = %q, want %q (non-head member is stale)", got, ExcludeReasonReplicaMemberUnreachable)
	}

	// Only the head node itself is stale: its own health already covers it,
	// this filter must not exclude it.
	setStaleByName(t, r, "worker", false)
	_, excluded, _ = r.filterCandidates(r.nodes, "", "", nil)
	if got, ok := excludedReasons(excluded)["head"]; ok {
		t.Errorf("head excluded (%q) because only its own node is stale", got)
	}
}

func TestOnlyHeadStaleWorkerFallsBackWithLabel(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	setStaleByName(t, r, "worker", true)

	picked, _, dec := r.Route("", "", "")
	if picked == nil || picked.Name != "head" {
		t.Fatalf("Route picked %v, want the head as last resort", picked)
	}
	if dec == nil || !strings.Contains(dec.Detail, "last resort") {
		t.Fatalf("decision detail %q must label the last-resort fallback", dec.Detail)
	}
	if got := excludedReasons(dec.Excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("explain reason for head = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}

	// RouteExcluding takes the same path.
	picked, _, dec = r.RouteExcluding("", "", nil)
	if picked == nil || picked.Name != "head" || !strings.Contains(dec.Detail, "last resort") {
		t.Errorf("RouteExcluding picked %v detail %q, want head with last-resort label", picked, dec.Detail)
	}

	// A caller-directed retry exclusion still wins over the fallback.
	picked, _, dec = r.RouteExcluding("", "", map[string]bool{"http://head.invalid:8000": true})
	if picked != nil || dec == nil || dec.Reason != ReasonNoCandidate {
		t.Errorf("retry-excluded head must give no_candidate, got %v %+v", picked, dec)
	}
}

func TestTwoHeadsOneStaleNoFallback(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
		{Name: "head2", URL: "http://head2.invalid:8000", Host: "h2"},
		{Name: "worker2", URL: "http://worker2.invalid:8000", Host: "h3"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	decl2 := peers("head2", "head2", "worker2")
	for _, name := range []string{"head2", "worker2"} {
		n := nodeByName(t, r, name)
		n.mu.Lock()
		n.ReplicaPeers = decl2
		n.mu.Unlock()
	}
	setStaleByName(t, r, "worker", true)

	picked, _, dec := r.Route("", "", "")
	if picked == nil || picked.Name != "head2" {
		t.Fatalf("Route picked %v, want head2", picked)
	}
	if strings.Contains(dec.Detail, "last resort") {
		t.Errorf("fallback must not trigger while a healthy head exists: %q", dec.Detail)
	}
	if got := excludedReasons(dec.Excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("stale head reason = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}
}

func TestStickyPinBypassedWhileWorkerStaleAndKeptOnRecovery(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "aa-head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
		{Name: "std", URL: "http://std.invalid:8000", Host: "h2"},
	}
	r := New(config.RoutingConfig{SessionAffinity: true}, cfgs, nil)
	decl := peers("aa-head", "aa-head", "worker")
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		if n.Name != "std" {
			n.ReplicaPeers = decl
		}
		n.mu.Unlock()
	}
	// Make std lose the alphabetical tiebreak so the first pin is the head.
	picked, _, _ := r.Route("", "sess", "")
	if picked == nil || picked.Name != "aa-head" {
		t.Fatalf("setup: first route picked %v, want aa-head", picked)
	}
	pinURL := func() string {
		r.affinityMu.Lock()
		defer r.affinityMu.Unlock()
		if e := r.affinity["sess"]; e != nil {
			return e.nodeURL
		}
		return ""
	}
	if pinURL() != "http://head.invalid:8000" {
		t.Fatalf("setup: pin = %q", pinURL())
	}

	setStaleByName(t, r, "worker", true)
	picked, _, dec := r.Route("", "sess", "")
	if picked == nil || picked.Name != "std" {
		t.Fatalf("while stale Route picked %v, want std", picked)
	}
	if !dec.AffinityLost {
		t.Error("bypassing the pin should report AffinityLost")
	}
	if pinURL() != "http://head.invalid:8000" {
		t.Errorf("pin = %q after bypass, want the original head pin kept", pinURL())
	}

	// Flap: stale -> clear -> stale keeps the original pin throughout.
	setStaleByName(t, r, "worker", false)
	picked, _, dec = r.Route("", "sess", "")
	if picked == nil || picked.Name != "aa-head" || dec.Reason != ReasonSessionAffinity {
		t.Fatalf("after recovery Route picked %v reason %v, want the pinned head via session_affinity", picked, dec)
	}
	setStaleByName(t, r, "worker", true)
	r.Route("", "sess", "")
	if pinURL() != "http://head.invalid:8000" {
		t.Errorf("pin = %q after a flap, want it unchanged", pinURL())
	}
}

func TestUnreachableReplicaHeadsHelper(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	setStaleByName(t, r, "worker", true)
	roles, heads := resolveSchedulingRolesAndHeads(r.nodes)

	// Member removed between role resolution and the read: fail open.
	var without []*NodeState
	for _, n := range r.nodes {
		if n.Name != "worker" {
			without = append(without, n)
		}
	}
	if down := unreachableReplicaHeads(without, roles, heads); down["head"] {
		t.Error("head marked down although its stale member is no longer in the node list")
	}
	if down := unreachableReplicaHeads(r.nodes, roles, heads); !down["head"] {
		t.Error("head not marked down for a stale member")
	}

	// No replica heads: nil, and no node lock is touched.
	standalone := nodeByName(t, r, "std")
	standalone.mu.Lock()
	done := make(chan map[string]bool, 1)
	go func() { done <- unreachableReplicaHeads(r.nodes, map[string]SchedulingRole{}, map[string]string{}) }()
	select {
	case got := <-done:
		if got != nil {
			t.Errorf("helper returned %v for no heads, want nil", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("helper blocked on a node lock although there are no replica heads")
	}
	standalone.mu.Unlock()
}

func TestFilterCandidatesRaceWithAgentStaleWrites(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	w := nodeByName(t, r, "worker")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		v := false
		for {
			select {
			case <-stop:
				return
			default:
				v = !v
				r.setAgentStale(w, v)
			}
		}
	}()
	for i := 0; i < 500; i++ {
		r.filterCandidates(r.nodes, "", "", nil)
		r.Route("", "sess", "")
	}
	close(stop)
	wg.Wait()
}

// Producer-driven cases: the flag is set by the real poll path.

func staleProducerRouter(t *testing.T, a *switchAgent) *Router {
	t.Helper()
	psSrv := nodePSServer()
	t.Cleanup(psSrv.Close)
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000, SessionAffinity: true}, []config.NodeConfig{
		{Name: "head", URL: psSrv.URL, Host: "head-host"},
		{Name: "worker", URL: psSrv.URL, Host: "127.0.0.1"},
		{Name: "std", URL: psSrv.URL, Host: "std-host"},
	}, nil)
	decl := peers("head", "head", "worker")
	for _, n := range r.nodes {
		n.mu.Lock()
		n.Healthy = true
		if n.Name != "std" {
			n.ReplicaPeers = decl
		}
		n.mu.Unlock()
	}
	r.SetMarborAgent("127.0.0.1", true, mustPort(t, a.srv.URL), "tok", "http")
	return r
}

func headExcludedNow(t *testing.T, r *Router) bool {
	t.Helper()
	_, excluded, _ := r.filterCandidates(r.nodes, "", "", nil)
	return excludedReasons(excluded)["head"] == ExcludeReasonReplicaMemberUnreachable
}

func TestStaleProducerFailedPollsExcludeHead(t *testing.T) {
	for name, mode := range map[string]agentMode{
		"non200":        modeDrop,
		"nonJSONOver":   modeGarbage,
		"malformedJSON": modeMalformed,
		"readError":     modeReadError,
	} {
		t.Run(name, func(t *testing.T) {
			a := newSwitchAgent(t)
			r := staleProducerRouter(t, a)
			r.pollAgentHosts()
			if headExcludedNow(t, r) {
				t.Fatal("head excluded after a good poll")
			}
			a.set(mode)
			for i := 0; i < r.healthFailureThreshold-1; i++ {
				r.pollAgentHosts()
			}
			if headExcludedNow(t, r) {
				t.Fatal("head excluded below the failure threshold")
			}
			r.pollAgentHosts()
			if !headExcludedNow(t, r) {
				t.Fatal("head not excluded after the failure threshold")
			}
			a.set(modeGood)
			r.pollAgentHosts()
			if headExcludedNow(t, r) {
				t.Fatal("head still excluded after the agent recovered")
			}
		})
	}
}

func TestStaleProducerOversizeJSONKeepsHeadRoutable(t *testing.T) {
	a := newSwitchAgent(t)
	r := staleProducerRouter(t, a)
	a.set(modeOversize)
	for i := 0; i < r.healthFailureThreshold+2; i++ {
		r.pollAgentHosts()
	}
	if headExcludedNow(t, r) {
		t.Fatal("over-cap JSON reply must count as reachable and keep the head routable")
	}

	// stale -> oversize JSON -> routable again.
	a.set(modeDrop)
	for i := 0; i < r.healthFailureThreshold; i++ {
		r.pollAgentHosts()
	}
	if !headExcludedNow(t, r) {
		t.Fatal("setup: head should be excluded after failed polls")
	}
	a.set(modeOversize)
	r.pollAgentHosts()
	if headExcludedNow(t, r) {
		t.Fatal("head still excluded after an over-cap JSON reply cleared the stale flag")
	}
}
