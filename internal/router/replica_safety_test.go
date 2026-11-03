package router

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	marboragent "github.com/Anirudhx7/marbor/internal/marboragent"
)

// safetyNode describes one node of a test fleet. A zero lastPoll means "polled
// just now"; set never to leave the node unpolled.
type safetyNode struct {
	name     string
	runtime  string
	models   []string
	down     bool // not healthy
	draining bool
	never    bool          // never polled
	age      time.Duration // how long ago the last poll landed
	peers    string        // replica head name, "" for standalone
	members  []string
	gpuShort bool // declared GPU group larger than the host reports
}

func safetyFleet(t *testing.T, specs ...safetyNode) *Router {
	t.Helper()
	cfgs := make([]config.NodeConfig, 0, len(specs))
	for _, s := range specs {
		cfgs = append(cfgs, config.NodeConfig{Name: s.name, URL: "http://" + s.name + ".invalid:8000", Runtime: s.runtime, Host: s.name + "-host"})
	}
	r := New(config.RoutingConfig{}, cfgs, nil)
	for i, s := range specs {
		n := r.nodes[i]
		n.mu.Lock()
		n.Runtime = s.runtime
		n.Healthy = !s.down
		n.Draining = s.draining
		if !s.never {
			n.LastPollAt = time.Now().Add(-s.age)
		}
		for _, m := range s.models {
			n.LoadedModels = append(n.LoadedModels, ModelInfo{Name: m})
		}
		if s.peers != "" {
			n.ReplicaPeers = peers(s.peers, s.members...)
		}
		if s.gpuShort {
			n.DeclaredGPUIndices = []int{0, 1}
			n.AgentGPUs = []marboragent.GPUInfo{{Index: 0}}
		}
		n.mu.Unlock()
	}
	return r
}

func impactOf(t *testing.T, r *Router, node string, down map[string]bool) SoleReplicaImpact {
	t.Helper()
	got, ok := r.SoleReplicaImpact(node, down)
	if !ok {
		t.Fatalf("node %q reported unknown", node)
	}
	return got
}

func modelNames(imp SoleReplicaImpact) []string {
	var out []string
	for _, m := range imp.Models {
		out = append(out, m.Model)
	}
	return out
}

func wantSole(t *testing.T, imp SoleReplicaImpact, sole ...string) {
	t.Helper()
	got := modelNames(imp)
	if fmt.Sprint(got) != fmt.Sprint(sole) {
		t.Fatalf("sole models = %v, want %v (full: %+v)", got, sole, imp)
	}
}

func TestSoleReplicaImpact_SoleAndCovered(t *testing.T) {
	const m = "org/model-a"
	tests := []struct {
		name     string
		fleet    []safetyNode
		sole     []string
		cold     int
		unknowns int
	}{
		{"sole vllm", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm"}}, []string{m}, 0, 0},
		{"warm same-runtime peer covers", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m}}}, nil, 0, 0},
		{"warm cross-runtime peer covers a non-ollama target", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "tgi", models: []string{m}}}, nil, 0, 0},
		{"draining peer does not count", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m}, draining: true}}, []string{m}, 0, 0},
		{"unhealthy peer does not count", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m}, down: true}}, []string{m}, 0, 0},
		{"worker peer does not count", []safetyNode{
			{name: "t", runtime: "vllm", models: []string{m}},
			{name: "h", runtime: "vllm", peers: "h", members: []string{"h", "w"}},
			{name: "w", runtime: "vllm", models: []string{m}, peers: "h", members: []string{"h", "w"}},
		}, []string{m}, 0, 0},
		{"ollama peer cannot cold-load for a vllm target", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "ollama"}}, []string{m}, 0, 0},
		{"tag mismatch on non-ollama is sole", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m + ":latest"}}}, []string{m}, 0, 0},
		{"tp peer with too few gpus is ineligible", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m}, gpuShort: true}}, []string{m}, 0, 0},
		{"cold ollama peer is cold-load only", []safetyNode{{name: "t", runtime: "ollama", models: []string{"llama3"}}, {name: "p", runtime: "ollama"}}, nil, 1, 0},
		{"empty runtime counts as ollama", []safetyNode{{name: "t", runtime: "", models: []string{"llama3"}}, {name: "p", runtime: ""}}, nil, 1, 0},
		{"warm ollama peer covers", []safetyNode{{name: "t", runtime: "ollama", models: []string{"llama3"}}, {name: "p", runtime: "ollama", models: []string{"llama3"}}}, nil, 0, 0},
		{"warm non-ollama peer is unknown for an ollama target", []safetyNode{{name: "t", runtime: "ollama", models: []string{"llama3"}}, {name: "p", runtime: "vllm", models: []string{"llama3"}}}, nil, 0, 1},
		{"never-polled peer is unknown", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", never: true}}, nil, 0, 1},
		{"stale peer is unknown", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "vllm", age: time.Hour}}, nil, 0, 1},
		{"auto peer is unknown", []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, {name: "p", runtime: "auto"}}, nil, 0, 1},
		{"auto target makes peers unknown", []safetyNode{{name: "t", runtime: "auto", models: []string{m}}, {name: "p", runtime: "vllm", models: []string{m}}}, nil, 0, 1},
		{"unknown plus cold reports cold only", []safetyNode{
			{name: "t", runtime: "ollama", models: []string{"llama3"}},
			{name: "c", runtime: "ollama"},
			{name: "u", runtime: "ollama", never: true},
		}, nil, 1, 0},
		{"unresolved replica peer is unknown", []safetyNode{
			{name: "t", runtime: "vllm", models: []string{m}},
			{name: "u", runtime: "vllm", peers: "u", members: []string{"u", "ghost"}},
		}, nil, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := safetyFleet(t, tc.fleet...)
			imp := impactOf(t, r, "t", nil)
			wantSole(t, imp, tc.sole...)
			if len(imp.ColdLoadOnly) != tc.cold {
				t.Errorf("cold = %d, want %d: %+v", len(imp.ColdLoadOnly), tc.cold, imp)
			}
			if len(imp.Unknown) != tc.unknowns {
				t.Errorf("unknown = %d, want %d: %+v", len(imp.Unknown), tc.unknowns, imp)
			}
		})
	}
}

func TestSoleReplicaImpact_TargetGate(t *testing.T) {
	const m = "org/model-a"
	peerNode := safetyNode{name: "p", runtime: "vllm"}
	t.Run("draining target has nothing to report", func(t *testing.T) {
		r := safetyFleet(t, safetyNode{name: "t", runtime: "vllm", models: []string{m}, draining: true}, peerNode)
		imp := impactOf(t, r, "t", nil)
		if len(imp.Models)+len(imp.ColdLoadOnly)+len(imp.Unknown) != 0 {
			t.Fatalf("want empty, got %+v", imp)
		}
	})
	t.Run("unhealthy target reports a node-level gap", func(t *testing.T) {
		r := safetyFleet(t, safetyNode{name: "t", runtime: "vllm", models: []string{m}, down: true}, peerNode)
		imp := impactOf(t, r, "t", nil)
		if len(imp.Models) != 0 || len(imp.Unknown) != 1 || imp.Unknown[0].Model != "" || imp.Unknown[0].Node != "t" {
			t.Fatalf("want one node-level unknown, got %+v", imp)
		}
	})
	t.Run("never-polled and stale targets report a gap", func(t *testing.T) {
		for _, sn := range []safetyNode{{name: "t", runtime: "vllm", models: []string{m}, never: true}, {name: "t", runtime: "vllm", models: []string{m}, age: time.Hour}} {
			imp := impactOf(t, safetyFleet(t, sn, peerNode), "t", nil)
			if len(imp.Models) != 0 || len(imp.Unknown) != 1 {
				t.Fatalf("want one gap, got %+v", imp)
			}
		}
	})
	t.Run("target mid-load serves nothing yet", func(t *testing.T) {
		r := safetyFleet(t, safetyNode{name: "t", runtime: "vllm"}, peerNode)
		imp := impactOf(t, r, "t", nil)
		if len(imp.Models)+len(imp.ColdLoadOnly)+len(imp.Unknown) != 0 {
			t.Fatalf("want empty, got %+v", imp)
		}
	})
	t.Run("worker and unresolved targets have nothing to report", func(t *testing.T) {
		r := safetyFleet(t,
			safetyNode{name: "h", runtime: "vllm", peers: "h", members: []string{"h", "w"}},
			safetyNode{name: "w", runtime: "vllm", models: []string{m}, peers: "h", members: []string{"h", "w"}},
			safetyNode{name: "u", runtime: "vllm", models: []string{m}, peers: "u", members: []string{"u", "ghost"}},
		)
		for _, name := range []string{"w", "u"} {
			imp := impactOf(t, r, name, nil)
			if len(imp.Models)+len(imp.ColdLoadOnly)+len(imp.Unknown) != 0 {
				t.Fatalf("%s: want empty, got %+v", name, imp)
			}
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		r := safetyFleet(t, peerNode)
		if _, ok := r.SoleReplicaImpact("nope", nil); ok {
			t.Fatal("unknown node must report false")
		}
	})
}

func TestSoleReplicaImpact_ReplicaHeads(t *testing.T) {
	const m = "org/big"
	members := []string{"h1", "w1"}
	t.Run("head target with a warm standalone peer is covered", func(t *testing.T) {
		r := safetyFleet(t,
			safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members},
			safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members},
			safetyNode{name: "solo", runtime: "vllm", models: []string{m}},
		)
		wantSole(t, impactOf(t, r, "h1", nil))
	})
	t.Run("head target with a warm head peer is covered", func(t *testing.T) {
		other := []string{"h2", "w2"}
		r := safetyFleet(t,
			safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members},
			safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members},
			safetyNode{name: "h2", runtime: "vllm", models: []string{m}, peers: "h2", members: other},
			safetyNode{name: "w2", runtime: "vllm", peers: "h2", members: other},
		)
		wantSole(t, impactOf(t, r, "h1", nil))
	})
	t.Run("head target whose only peer head has no warm copy is sole", func(t *testing.T) {
		other := []string{"h2", "w2"}
		r := safetyFleet(t,
			safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members},
			safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members},
			safetyNode{name: "h2", runtime: "vllm", peers: "h2", members: other},
			safetyNode{name: "w2", runtime: "vllm", peers: "h2", members: other},
		)
		wantSole(t, impactOf(t, r, "h1", nil), m)
	})
	t.Run("head peer with an unreachable worker is unknown not sole", func(t *testing.T) {
		r := safetyFleet(t,
			safetyNode{name: "t", runtime: "vllm", models: []string{m}},
			safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members},
			safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members},
		)
		r.setAgentStale(nodeByName(t, r, "w1"), true)
		imp := impactOf(t, r, "t", nil)
		if len(imp.Models) != 0 || len(imp.Unknown) != 1 || imp.Unknown[0].Node != "h1" {
			t.Fatalf("want one unknown on h1, got %+v", imp)
		}
	})
}

func TestSoleReplicaImpact_AlsoDown(t *testing.T) {
	const m = "org/model-a"
	r := safetyFleet(t,
		safetyNode{name: "a", runtime: "vllm", models: []string{m}},
		safetyNode{name: "b", runtime: "vllm", models: []string{m}},
	)
	wantSole(t, impactOf(t, r, "a", nil))
	wantSole(t, impactOf(t, r, "a", map[string]bool{"b": true}), m)
}

func TestSoleReplicaImpact_DigestMismatchIsNotWarm(t *testing.T) {
	const m = "llama3"
	r := safetyFleet(t,
		safetyNode{name: "t", runtime: "vllm", models: []string{m}},
		safetyNode{name: "p", runtime: "vllm", models: []string{m}},
	)
	nodeByName(t, r, "p").LoadedModels[0].Digest = "sha-b"
	r.digestMu.Lock()
	r.modelDigests[m] = "sha-a"
	r.digestMu.Unlock()
	wantSole(t, impactOf(t, r, "t", nil), m)
}

func TestSoleReplicaImpact_SaturatedPeerStillCounts(t *testing.T) {
	const m = "org/model-a"
	r := safetyFleet(t,
		safetyNode{name: "t", runtime: "vllm", models: []string{m}},
		safetyNode{name: "p", runtime: "vllm", models: []string{m}},
	)
	p := nodeByName(t, r, "p")
	p.MaxInFlight = 1
	p.ActiveConns = 5
	wantSole(t, impactOf(t, r, "t", nil))
}

// The check must agree with routing about which peers are eligible, except
// where it deliberately differs: a busy peer still counts, an unwarm Ollama
// peer is only a cold-load candidate for an Ollama target, a peer's data can
// be unknown, and a warm non-Ollama peer is unknown for an Ollama target.
func TestSoleReplicaImpact_ParityWithRouting(t *testing.T) {
	const m = "shared-model"
	runtimes := []string{"ollama", "", "vllm", "tgi"}
	for _, tr := range runtimes {
		for _, pr := range runtimes {
			for _, down := range []bool{false, true} {
				for _, drain := range []bool{false, true} {
					for _, warm := range []bool{false, true} {
						for _, short := range []bool{false, true} {
							checkParity(t, m, tr, pr, down, drain, warm, short)
						}
					}
				}
			}
		}
	}
}

func checkParity(t *testing.T, m, targetRT, peerRT string, down, drain, warm, short bool) {
	t.Helper()
	peer := safetyNode{name: "p", runtime: peerRT, down: down, draining: drain, gpuShort: short}
	if warm {
		peer.models = []string{m}
	}
	r := safetyFleet(t, safetyNode{name: "t", runtime: targetRT, models: []string{m}}, peer)
	label := fmt.Sprintf("target=%q peer=%q down=%v drain=%v warm=%v short=%v", targetRT, peerRT, down, drain, warm, short)
	tn, pn := nodeByName(t, r, "t"), nodeByName(t, r, "p")
	tv, pv := snapshotSafetyNode(tn), snapshotSafetyNode(pn)
	roles, _ := resolveSchedulingRolesAndHeads(r.nodes)
	verdict, _ := r.judgePeer(pv, m, tv.runtime, roles["p"], false)
	for _, filter := range []string{"", "ollama"} {
		f := r.filterCandidatesWithFallback([]*NodeState{pn}, m, filter, nil)
		routable := len(f.healthy) == 1
		switch verdict {
		case verdictWarm, verdictCold, verdictUnknown:
			// Routing may not use the peer under a runtime filter that
			// excludes it, but with no filter an eligible verdict must be
			// routable.
			if filter == "" && !routable {
				t.Errorf("%s filter=%q: verdict %d but routing rejects the peer", label, filter, verdict)
			}
		case verdictIneligible:
			// The only allowed divergence: routing accepts an unwarm Ollama
			// peer for any model, the check only for an Ollama target.
			if routable && !(pv.runtime == runtimeOllama && !warm && tv.runtime != runtimeOllama) {
				t.Errorf("%s filter=%q: routing accepts the peer but the check calls it ineligible", label, filter)
			}
		}
	}
}

// The same parity check for peers that are replica members or stale: a worker
// is never a server, a stale peer or a head with an unreachable worker is
// reported unknown (routing may or may not use it, the check refuses to guess),
// and a healthy warm head is covered and routable.
func TestSoleReplicaImpact_ParityWithRoutingReplicaAndStalePeers(t *testing.T) {
	const m = "shared-model"
	members := []string{"h1", "w1"}
	type outcome int
	const (
		covered outcome = iota
		unknown
		sole
	)
	cases := []struct {
		name        string
		peer        safetyNode
		worker      *safetyNode
		staleWorker bool
		want        outcome
		peerName    string
	}{
		{name: "stale warm peer", peer: safetyNode{name: "p", runtime: "vllm", models: []string{m}, age: 24 * time.Hour}, want: unknown, peerName: "p"},
		{name: "never polled peer", peer: safetyNode{name: "p", runtime: "vllm", models: []string{m}, never: true}, want: unknown, peerName: "p"},
		{name: "healthy warm head", peer: safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members}, worker: &safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members}, want: covered, peerName: "h1"},
		{name: "warm head with an unreachable worker", peer: safetyNode{name: "h1", runtime: "vllm", models: []string{m}, peers: "h1", members: members}, worker: &safetyNode{name: "w1", runtime: "vllm", peers: "h1", members: members}, staleWorker: true, want: unknown, peerName: "h1"},
		{name: "worker is never a server", peer: safetyNode{name: "w1", runtime: "vllm", models: []string{m}, peers: "h1", members: members}, worker: &safetyNode{name: "h1", runtime: "vllm", peers: "h1", members: members, down: true}, want: sole, peerName: "w1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specs := []safetyNode{{name: "t", runtime: "vllm", models: []string{m}}, tc.peer}
			if tc.worker != nil {
				specs = append(specs, *tc.worker)
			}
			r := safetyFleet(t, specs...)
			if tc.staleWorker {
				r.setAgentStale(nodeByName(t, r, "w1"), true)
			}
			imp := impactOf(t, r, "t", nil)
			var got outcome
			switch {
			case len(imp.Models) > 0:
				got = sole
			case len(imp.Unknown) > 0:
				got = unknown
			default:
				got = covered
			}
			if got != tc.want {
				t.Fatalf("outcome = %v, want %v (impact %+v)", got, tc.want, imp)
			}
			// Routing never sends traffic to a node the check calls covered
			// unless routing itself can use it.
			if tc.want == covered {
				if !routes(r, tc.peerName, m) {
					t.Errorf("the check counts %s as a server but routing rejects it", tc.peerName)
				}
			}
			// A peer routing accepts is never reported as a sole-replica gap.
			if tc.want == sole {
				if routes(r, tc.peerName, m) {
					t.Errorf("routing accepts %s but the check reports the model as sole", tc.peerName)
				}
			}
		})
	}
}

// routes reports whether routing would consider the named node for model m,
// judged among the whole fleet (replica roles need the other members).
func routes(r *Router, name, m string) bool {
	f := r.filterCandidatesWithFallback(r.nodes, m, "", nil)
	for _, n := range f.healthy {
		if n.Name == name {
			return true
		}
	}
	return false
}

func TestSoleReplicaImpact_ConcurrentRaceClean(t *testing.T) {
	r := safetyFleet(t,
		safetyNode{name: "a", runtime: "vllm", models: []string{"x"}},
		safetyNode{name: "b", runtime: "vllm", models: []string{"x"}},
	)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%2 == 0 {
					r.SoleReplicaImpact("a", nil)
					continue
				}
				n := nodeByName(t, r, "b")
				n.mu.Lock()
				n.Draining = j%2 == 0
				n.mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
}

func TestSoleReplicaImpact_SortedAndDeduplicated(t *testing.T) {
	r := safetyFleet(t,
		safetyNode{name: "t", runtime: "vllm", models: []string{"zeta", "alpha", "alpha"}},
		safetyNode{name: "p", runtime: "vllm"},
	)
	wantSole(t, impactOf(t, r, "t", nil), "alpha", "zeta")
}
