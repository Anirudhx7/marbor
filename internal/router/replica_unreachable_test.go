package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// filterCandidatesNoFallback is the hard filter without the last-resort
// list, for tests that only care about the healthy and excluded sets. It is
// a test helper only; production code calls filterCandidatesWithFallback.
func (r *Router) filterCandidatesNoFallback(nodes []*NodeState, modelName, runtimeFilter string, exclude map[string]bool) (healthy []*NodeState, excluded []ExcludedCandidate, excludedTotal int) {
	f := r.filterCandidatesWithFallback(nodes, modelName, runtimeFilter, exclude)
	return f.healthy, f.excluded, f.excludedTotal
}

// pinSession installs a session-affinity entry for sess on the named node,
// without going through scoring.
func pinSession(t *testing.T, r *Router, sess, node string) {
	t.Helper()
	n := nodeByName(t, r, node)
	entry := &affinityEntry{nodeURL: n.URL}
	entry.lastSeen.Store(time.Now().UnixNano())
	r.affinityMu.Lock()
	r.affinity[sess] = entry
	r.affinityMu.Unlock()
}

func pinnedURL(r *Router, sess string) string {
	r.affinityMu.Lock()
	defer r.affinityMu.Unlock()
	if e := r.affinity[sess]; e != nil {
		return e.nodeURL
	}
	return ""
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

	healthy, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	healthy, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
	if got := excludedReasons(excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("co-hosted head reason = %q, want %q (non-head member is stale)", got, ExcludeReasonReplicaMemberUnreachable)
	}

	// Only the head node itself is stale: its own health already covers it,
	// this filter must not exclude it.
	setStaleByName(t, r, "worker", false)
	_, excluded, _ = r.filterCandidatesNoFallback(r.nodes, "", "", nil)
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
		t.Fatalf("decision %+v must label the last-resort fallback", dec)
	}
	if got := excludedReasons(dec.Excluded)["head"]; got != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("explain reason for head = %q, want %q", got, ExcludeReasonReplicaMemberUnreachable)
	}

	// RouteExcluding takes the same path.
	picked, _, dec = r.RouteExcluding("", "", nil)
	if picked == nil || picked.Name != "head" || dec == nil || !strings.Contains(dec.Detail, "last resort") {
		t.Fatalf("RouteExcluding picked %v decision %+v, want head with last-resort label", picked, dec)
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

// stickyReplicaRouter is a replica headed by "aa-head" (worker "worker") plus
// a standalone "std", with sess pinned to the head explicitly so the tests do
// not depend on how a first request would have been scored.
func stickyReplicaRouter(t *testing.T) *Router {
	t.Helper()
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
	pinSession(t, r, "sess", "aa-head")
	return r
}

func TestStickyPinBypassedWhileWorkerStaleAndKeptOnRecovery(t *testing.T) {
	r := stickyReplicaRouter(t)
	const headURL = "http://head.invalid:8000"

	setStaleByName(t, r, "worker", true)
	picked, _, dec := r.Route("", "sess", "")
	if picked == nil || picked.Name != "std" {
		t.Fatalf("while stale Route picked %v, want std", picked)
	}
	if !dec.AffinityLost {
		t.Error("bypassing the pin should report AffinityLost")
	}
	if !strings.Contains(dec.Detail, "session pin kept but skipped") {
		t.Errorf("detail %q should say the pin was kept but skipped", dec.Detail)
	}
	if got := pinnedURL(r, "sess"); got != headURL {
		t.Errorf("pin = %q after bypass, want the original head pin kept", got)
	}

	setStaleByName(t, r, "worker", false)
	picked, _, dec = r.Route("", "sess", "")
	if picked == nil || picked.Name != "aa-head" || dec.Reason != ReasonSessionAffinity {
		t.Fatalf("after recovery Route picked %v reason %v, want the pinned head via session_affinity", picked, dec)
	}
}

// TestStickyPinFlapKeepsOriginalPin walks stale -> clear -> stale -> clear and
// checks the node picked after each step as well as the unchanged pin. The pin
// is installed explicitly on the head, so the head is the sticky target
// whenever the worker's agent answers, with no reliance on a scoring tiebreak.
func TestStickyPinFlapKeepsOriginalPin(t *testing.T) {
	r := stickyReplicaRouter(t)
	const headURL = "http://head.invalid:8000"
	steps := []struct {
		stale    bool
		wantNode string
	}{
		{true, "std"},
		{false, "aa-head"},
		{true, "std"},
		{false, "aa-head"},
	}
	for i, st := range steps {
		setStaleByName(t, r, "worker", st.stale)
		picked, _, _ := r.Route("", "sess", "")
		if picked == nil || picked.Name != st.wantNode {
			t.Fatalf("step %d (stale=%v): picked %v, want %s", i, st.stale, picked, st.wantNode)
		}
		if got := pinnedURL(r, "sess"); got != headURL {
			t.Fatalf("step %d: pin = %q, want %q unchanged", i, got, headURL)
		}
	}
}

func TestStickyPinSoleHeadStaleWorkerRoutesToHeadAndKeepsPin(t *testing.T) {
	cfgs := []config.NodeConfig{
		{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
		{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
	}
	r := replicaStaleRouter(t, cfgs, "head", "worker")
	pinSession(t, r, "sess", "head")
	setStaleByName(t, r, "worker", true)

	picked, _, dec := r.Route("", "sess", "")
	if picked == nil || picked.Name != "head" {
		t.Fatalf("Route picked %v, want the pinned head as last resort", picked)
	}
	if got := pinnedURL(r, "sess"); got != "http://head.invalid:8000" {
		t.Errorf("pin = %q, want it unchanged", got)
	}
	if !dec.AffinityLost {
		t.Error("AffinityLost should be set when the pin was bypassed")
	}
	if !strings.HasSuffix(dec.Detail, "the session pin was skipped for this request and kept)") {
		t.Errorf("detail %q should end by saying the pin was skipped for this request and kept", dec.Detail)
	}
	if n := strings.Count(dec.Detail, "last resort"); n != 1 {
		t.Errorf("detail %q mentions last resort %d times, want once", dec.Detail, n)
	}
	if strings.Contains(dec.Detail, "no longer usable") {
		t.Errorf("detail %q must not carry the generic lost-pin wording", dec.Detail)
	}
}

func TestAffinityLostDetailHasNoReplicaWordingForPlainLoss(t *testing.T) {
	r := stickyReplicaRouter(t)
	pinSession(t, r, "plain", "std")
	std := nodeByName(t, r, "std")
	std.mu.Lock()
	std.Healthy = false
	std.mu.Unlock()

	picked, _, dec := r.Route("", "plain", "")
	if picked == nil || dec == nil || !dec.AffinityLost {
		t.Fatalf("Route picked %v decision %+v, want a pick with AffinityLost", picked, dec)
	}
	if strings.Contains(dec.Detail, "replica") {
		t.Errorf("detail %q must not mention replicas when the pin was lost for another reason", dec.Detail)
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
	if down := unreachableReplicaHeads(without, roles, heads); down["head"] != nil {
		t.Error("head marked down although its stale member is no longer in the node list")
	}
	down := unreachableReplicaHeads(r.nodes, roles, heads)
	if down["head"] == nil || down["head"].Name != "worker" {
		t.Errorf("head not marked down by its stale member worker: %v", down["head"])
	}

	// No replica heads: nil. This only checks the result; the early return
	// is a fast path, and a pass without heads would also return nil because
	// every worker lookup finds no head, so it is not observable from here.
	if got := unreachableReplicaHeads(r.nodes, roles, map[string]string{}); got != nil {
		t.Errorf("helper returned %v for no heads, want nil", got)
	}
}

// TestFilterCandidatesRaceWithAgentStaleWrites makes no assertion about the
// picked node inside the loop: the race detector is the check. The sticky
// path is confirmed to be taken before the loop starts.
func TestFilterCandidatesRaceWithAgentStaleWrites(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker")
	w := nodeByName(t, r, "worker")
	if picked, _, _ := r.Route("", "sess", ""); picked == nil {
		t.Fatal("setup: first route found no node")
	}
	if pinnedURL(r, "sess") == "" {
		t.Fatal("setup: the session was not pinned")
	}
	if _, _, dec := r.Route("", "sess", ""); dec == nil || dec.Reason != ReasonSessionAffinity {
		t.Fatalf("setup: second route %+v did not take the sticky path", dec)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	defer func() {
		close(stop)
		wg.Wait()
	}()
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
				runtime.Gosched()
			}
		}
	}()
	for i := 0; i < 500; i++ {
		r.filterCandidatesNoFallback(r.nodes, "", "", nil)
		r.Route("", "sess", "")
	}
}

// Producer-driven cases: the flag is set by the real poll path.

// staleProducerRouter builds head/worker/std where the worker shares the
// agent's host (127.0.0.1) and the agent listens on agentPort over scheme.
func staleProducerRouter(t *testing.T, agentPort int, scheme string) *Router {
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
	r.SetMarborAgent("127.0.0.1", true, agentPort, "tok", scheme)
	return r
}

func headExcludedNow(t *testing.T, r *Router) bool {
	t.Helper()
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
	return excludedReasons(excluded)["head"] == ExcludeReasonReplicaMemberUnreachable
}

// staleProducerCase builds a router wired to a failing-on-demand agent:
// fail makes every poll fail the designed way, heal makes it answer again.
type staleProducerCase struct {
	name  string
	build func(t *testing.T) (r *Router, fail, heal func())
}

func switchAgentCase(name string, mode agentMode) staleProducerCase {
	return staleProducerCase{name: name, build: func(t *testing.T) (*Router, func(), func()) {
		a := newSwitchAgent(t)
		r := staleProducerRouter(t, mustPort(t, a.srv.URL), "http")
		return r, func() { a.set(mode) }, func() { a.set(modeGood) }
	}}
}

func staleProducerCases() []staleProducerCase {
	return []staleProducerCase{
		switchAgentCase("non200", modeDrop),
		switchAgentCase("nonJSONOver", modeGarbage),
		switchAgentCase("malformedJSON", modeMalformed),
		switchAgentCase("readError", modeReadError),
		{name: "tokenRotation401", build: func(t *testing.T) (*Router, func(), func()) {
			// The agent only accepts "tok"; rotating the token marbor holds
			// makes every poll a 401, which the poller treats as a failure.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Authorization") != "Bearer tok" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(richTelemetry())
			}))
			t.Cleanup(srv.Close)
			port := mustPort(t, srv.URL)
			r := staleProducerRouter(t, port, "http")
			return r,
				func() { r.SetMarborAgent("127.0.0.1", true, port, "rotated", "http") },
				func() { r.SetMarborAgent("127.0.0.1", true, port, "tok", "http") }
		}},
		{name: "tlsPinMismatch", build: func(t *testing.T) (*Router, func(), func()) {
			// A pinned fingerprint that does not match the agent's certificate
			// fails the dial before any request; the poller counts that as
			// unreachable like any other failure.
			srv := agentTLSTelemetryServer()
			t.Cleanup(srv.Close)
			r := staleProducerRouter(t, mustPort(t, srv.URL), "https")
			pin := func(fp string) {
				if !r.PatchNode("worker", NodePatch{TLSFingerprint: &fp}) {
					t.Fatal("PatchNode returned false")
				}
				// The pin is checked when a connection is dialed, so drop
				// the kept-alive connection from the previous poll.
				r.client.CloseIdleConnections()
			}
			good := certFingerprint(t, srv)
			pin(good)
			return r, func() { pin("SHA256:" + strings.Repeat("0", 64)) }, func() { pin(good) }
		}},
	}
}

func TestStaleProducerFailedPollsExcludeHead(t *testing.T) {
	for _, tc := range staleProducerCases() {
		t.Run(tc.name, func(t *testing.T) {
			r, fail, heal := tc.build(t)
			r.pollAgentHosts()
			if headExcludedNow(t, r) {
				t.Fatal("head excluded after a good poll")
			}
			fail()
			// Every poll after fail() must be counted as a failure, so a
			// kept-alive connection or a cached answer cannot pass this test
			// for the wrong reason.
			worker := nodeByName(t, r, "worker")
			failures := func() int {
				worker.mu.RLock()
				defer worker.mu.RUnlock()
				return worker.AgentFailures
			}
			for i := 0; i < r.healthFailureThreshold-1; i++ {
				r.pollAgentHosts()
				if got := failures(); got != i+1 {
					t.Fatalf("poll %d after fail() left %d counted failures, want %d", i+1, got, i+1)
				}
			}
			if headExcludedNow(t, r) {
				t.Fatal("head excluded below the failure threshold")
			}
			r.pollAgentHosts()
			// The counter resets once the threshold is crossed (the agent is
			// marked stale), so only the stale flag is checked here.
			worker.mu.RLock()
			stale := worker.AgentStale
			worker.mu.RUnlock()
			if !stale {
				t.Fatal("worker not marked stale by the threshold poll")
			}
			if !headExcludedNow(t, r) {
				t.Fatal("head not excluded after the failure threshold")
			}
			heal()
			r.pollAgentHosts()
			if headExcludedNow(t, r) {
				t.Fatal("head still excluded after the agent recovered")
			}
		})
	}
}

func TestStaleProducerOversizeJSONKeepsHeadRoutable(t *testing.T) {
	a := newSwitchAgent(t)
	r := staleProducerRouter(t, mustPort(t, a.srv.URL), "http")
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

// TestStaleProducerAgentlessWorkerKeepsHead drives the agentless case through
// the real poll: a worker whose host has no agent configured (or whose agent
// was disabled after going dark) is unknown, never stale.
func TestStaleProducerAgentlessWorkerKeepsHead(t *testing.T) {
	t.Run("never configured", func(t *testing.T) {
		r := staleProducerRouter(t, 1, "http")
		r.SetMarborAgent("127.0.0.1", false, 0, "", "")
		for i := 0; i < r.healthFailureThreshold+1; i++ {
			r.pollAgentHosts()
		}
		if headExcludedNow(t, r) {
			t.Fatal("head excluded although the worker host has no agent")
		}
	})
	t.Run("disabled after going dark", func(t *testing.T) {
		a := newSwitchAgent(t)
		r := staleProducerRouter(t, mustPort(t, a.srv.URL), "http")
		a.set(modeDrop)
		for i := 0; i < r.healthFailureThreshold; i++ {
			r.pollAgentHosts()
		}
		if !headExcludedNow(t, r) {
			t.Fatal("setup: head should be excluded after failed polls")
		}
		r.SetMarborAgent("127.0.0.1", false, 0, "", "")
		r.pollAgentHosts()
		if headExcludedNow(t, r) {
			t.Fatal("head still excluded after the worker's agent was disabled")
		}
	})
}

// TestHeadWithDeclaredMemberAbsentFromFleet: a declaration naming a node that
// is not in the fleet cannot resolve, so the group is unresolved rather than a
// replica with an unreachable member, and nothing panics.
func TestHeadWithDeclaredMemberAbsentFromFleet(t *testing.T) {
	r := replicaStaleRouter(t, twoNodeCfgs(), "head", "worker", "ghost")
	setStaleByName(t, r, "worker", true)
	_, excluded, _ := r.filterCandidatesNoFallback(r.nodes, "", "", nil)
	reasons := excludedReasons(excluded)
	for _, name := range []string{"head", "worker"} {
		if got := reasons[name]; got != ExcludeReasonReplicaUnresolved {
			t.Errorf("%s reason = %q, want %q", name, got, ExcludeReasonReplicaUnresolved)
		}
	}
}

func TestUnhealthyOrDrainingHeadWithStaleWorkerIsNotResurrected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(n *NodeState)
	}{
		{"unhealthy", func(n *NodeState) { n.Healthy = false }},
		{"draining", func(n *NodeState) { n.Draining = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgs := []config.NodeConfig{
				{Name: "head", URL: "http://head.invalid:8000", Host: "h0"},
				{Name: "worker", URL: "http://worker.invalid:8000", Host: "h1"},
			}
			r := replicaStaleRouter(t, cfgs, "head", "worker")
			setStaleByName(t, r, "worker", true)
			h := nodeByName(t, r, "head")
			h.mu.Lock()
			tc.mutate(h)
			h.mu.Unlock()

			picked, _, dec := r.Route("", "", "")
			if picked != nil || dec == nil || dec.Reason != ReasonNoCandidate {
				t.Fatalf("Route picked %v decision %+v, want no_candidate (the last resort only covers the unreachable reason)", picked, dec)
			}
			if strings.Contains(dec.Detail, "last resort") {
				t.Errorf("detail %q must not claim a last resort", dec.Detail)
			}
		})
	}
}

func TestTwoStaleHeadsBothEnterLastResort(t *testing.T) {
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
	setStaleByName(t, r, "worker2", true)

	f := r.filterCandidatesWithFallback(r.nodes, "", "", nil)
	healthy, lastResort := f.healthy, f.lastResort
	if len(healthy) != 0 {
		t.Fatalf("healthy = %d nodes, want none", len(healthy))
	}
	names := map[string]bool{}
	for _, n := range lastResort {
		names[n.Name] = true
	}
	if len(lastResort) != 2 || !names["head"] || !names["head2"] {
		t.Fatalf("lastResort = %v, want both heads", names)
	}

	picked, _, dec := r.Route("", "", "")
	if picked == nil || dec == nil {
		t.Fatalf("Route picked %v decision %+v, want one of the two heads", picked, dec)
	}
	if picked.Name != "head" && picked.Name != "head2" {
		t.Fatalf("Route picked %q, want exactly one of head or head2", picked.Name)
	}
	reasons := excludedReasons(dec.Excluded)
	if reasons["head"] != ExcludeReasonReplicaMemberUnreachable || reasons["head2"] != ExcludeReasonReplicaMemberUnreachable {
		t.Errorf("excluded = %v, want both heads listed as unreachable", reasons)
	}
}
