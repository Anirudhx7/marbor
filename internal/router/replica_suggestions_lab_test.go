package router

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/store"
)

// fakeAgent is an in-process agent: a real HTTP server answering /v1/status
// with whatever telemetry the test sets, or a 503 while down.
type fakeAgent struct {
	mu   sync.Mutex
	down bool
	tel  marboragent.Telemetry
	srv  *httptest.Server
}

func newFakeAgent(t *testing.T, hostname string, addrs []string, deps ...marboragent.DeploymentReport) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{tel: marboragent.Telemetry{
		Agent:        marboragent.Agent{Version: "test", ProtocolVersion: 1},
		Capabilities: []string{"status", "deployment.report", capTopology},
		Host:         &marboragent.HostTelemetry{Hostname: hostname, Addrs: addrs},
		Deployments:  deps,
	}}
	fa.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fa.mu.Lock()
		down, tel := fa.down, fa.tel
		fa.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(tel)
	}))
	t.Cleanup(fa.srv.Close)
	return fa
}

func (fa *fakeAgent) setDown(v bool) {
	fa.mu.Lock()
	fa.down = v
	fa.mu.Unlock()
}

// twoHostLab wires a real Router with a vLLM head on host 127.0.0.1 and a
// headless worker on host localhost, each with its own fake agent.
func twoHostLab(t *testing.T) (*Router, *fakeAgent, *fakeAgent) {
	t.Helper()
	headAgent := newFakeAgent(t, "head-box", []string{"10.0.0.1"}, vllmDep(8000, 0, 2, "10.0.0.1", 29500, false))
	workerAgent := newFakeAgent(t, "worker-box", []string{"10.0.0.2"}, vllmDep(0, 1, 2, "10.0.0.1", 29500, true))
	r := New(config.RoutingConfig{PollIntervalMs: 2000}, []config.NodeConfig{
		{Name: "head", URL: "http://127.0.0.1:8000", Runtime: "vllm"},
		{Name: "worker", URL: "http://localhost:8000", Runtime: "vllm"},
	}, nil)
	r.SetMarborAgent("127.0.0.1", true, mustPort(t, headAgent.srv.URL), "tok", "http")
	r.SetMarborAgent("localhost", true, mustPort(t, workerAgent.srv.URL), "tok", "http")
	return r, headAgent, workerAgent
}

func TestReplicaLab_PollProducesCompleteSuggestion(t *testing.T) {
	r, _, _ := twoHostLab(t)
	r.pollAgentHosts()

	sugg, cov := r.ReplicaSuggestions()
	if len(sugg) != 1 || sugg[0].State != SuggestionComplete || !sugg[0].Confirmable {
		t.Fatalf("suggestions = %+v", sugg)
	}
	if sugg[0].Head != "head" || !reflect.DeepEqual(sugg[0].Members, []string{"head", "worker"}) {
		t.Errorf("group = head %q members %v", sugg[0].Head, sugg[0].Members)
	}
	for _, c := range cov {
		if c.State != CoverageReporting || !c.Detected {
			t.Errorf("coverage %+v", c)
		}
	}
}

func TestReplicaLab_EvidenceOutlivesOneBlipButNotTheThreshold(t *testing.T) {
	r, _, worker := twoHostLab(t)
	r.pollAgentHosts()
	worker.setDown(true)

	for i := 0; i < r.healthFailureThreshold-1; i++ {
		r.pollAgentHosts()
		if sugg, _ := r.ReplicaSuggestions(); len(sugg) != 1 || sugg[0].State != SuggestionComplete {
			t.Fatalf("after %d failed poll(s) the evidence must still stand: %+v", i+1, sugg)
		}
	}
	r.pollAgentHosts()
	sugg, cov := r.ReplicaSuggestions()
	if len(sugg) != 1 || sugg[0].State != SuggestionIncomplete || sugg[0].Confirmable {
		t.Fatalf("after the threshold the group must fall back to incomplete: %+v", sugg)
	}
	for _, c := range cov {
		if c.Node == "worker" && c.State != CoverageNoAgent {
			t.Errorf("worker coverage = %+v", c)
		}
	}
	// Recovery brings it back.
	worker.setDown(false)
	r.pollAgentHosts()
	if sugg, _ := r.ReplicaSuggestions(); len(sugg) != 1 || sugg[0].State != SuggestionComplete {
		t.Fatalf("after recovery: %+v", sugg)
	}
}

func TestReplicaLab_HostWithNoRowsLosesItsEvidenceAfterThePass(t *testing.T) {
	r, _, _ := twoHostLab(t)
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()["localhost"]; !ok {
		t.Fatal("precondition: worker host evidence recorded")
	}

	r.RemoveNode("worker") // last row on host "localhost"
	r.pollAgentHosts()

	if _, ok := r.snapshotHostEvidence()["localhost"]; ok {
		t.Error("evidence for a host with no remaining rows must be dropped after the next pass")
	}
	sugg, _ := r.ReplicaSuggestions()
	if len(sugg) != 1 || sugg[0].State != SuggestionIncomplete || len(sugg[0].Members) != 1 {
		t.Errorf("head alone must now be incomplete: %+v", sugg)
	}
}

func TestReplicaLab_DisabledAgentDropsEvidence(t *testing.T) {
	r, _, _ := twoHostLab(t)
	r.pollAgentHosts()
	r.SetMarborAgent("localhost", false, 0, "", "http")
	r.pollAgentHosts()
	if _, ok := r.snapshotHostEvidence()["localhost"]; ok {
		t.Error("evidence must be dropped when the host's agent is disabled")
	}
}

func TestRecordHostEvidence_StoresADeepCopy(t *testing.T) {
	r := New(config.RoutingConfig{}, nil, nil)
	dep := vllmDep(8000, 0, 2, "10.0.0.1", 29500, false)
	addrs := []string{"10.0.0.1"}
	r.RecordHostEvidence(HostEvidence{Host: "h", Addrs: addrs, Deployments: []marboragent.DeploymentReport{dep}})
	addrs[0] = "mutated"
	*dep.Topology.NNodes = 99
	got := r.snapshotHostEvidence()["h"]
	if got.Addrs[0] != "10.0.0.1" || *got.Deployments[0].Topology.NNodes != 2 {
		t.Errorf("stored evidence aliases the caller's data: %+v", got)
	}
	r.DropHostEvidence("h")
	if len(r.snapshotHostEvidence()) != 0 {
		t.Error("DropHostEvidence left an entry")
	}
}

func TestApplyReplicaPeersBatch(t *testing.T) {
	newR := func() *Router {
		return New(config.RoutingConfig{}, []config.NodeConfig{
			{Name: "a", URL: "http://a:8000", Runtime: "vllm"},
			{Name: "b", URL: "http://b:8000", Runtime: "vllm"},
		}, nil)
	}
	assign := map[string]store.ReplicaPeers{
		"a": {Members: []string{"a", "b"}, Head: "a"},
		"b": {Members: []string{"a", "b"}, Head: "a"},
	}
	declared := func(r *Router, name string) *store.ReplicaPeers {
		for _, n := range r.Nodes() {
			if n.Name == name {
				n.RLock()
				defer n.RUnlock()
				return n.ReplicaPeers
			}
		}
		return nil
	}

	t.Run("persists first then sets every member", func(t *testing.T) {
		r := newR()
		var persisted map[string]store.ReplicaPeers
		err := r.ApplyReplicaPeersBatch(assign, func(m map[string]store.ReplicaPeers) error {
			if declared(r, "a") != nil || declared(r, "b") != nil {
				t.Error("memory was set before the store was written")
			}
			persisted = m
			return nil
		})
		if err != nil || len(persisted) != 2 {
			t.Fatalf("err=%v persisted=%v", err, persisted)
		}
		if got := declared(r, "b"); got == nil || got.Head != "a" || len(got.Members) != 2 {
			t.Errorf("b = %+v", got)
		}
		// The router keeps its own copy.
		assign["a"].Members[0] = "x"
		if got := declared(r, "a"); got.Members[0] != "a" {
			t.Errorf("declaration aliases the caller's slice: %+v", got)
		}
		assign["a"].Members[0] = "a"
	})
	t.Run("store failure leaves memory unchanged", func(t *testing.T) {
		r := newR()
		boom := errors.New("disk full")
		if err := r.ApplyReplicaPeersBatch(assign, func(map[string]store.ReplicaPeers) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if declared(r, "a") != nil || declared(r, "b") != nil {
			t.Error("memory changed although the store write failed")
		}
	})
	t.Run("a missing member writes nothing anywhere", func(t *testing.T) {
		r := newR()
		called := false
		bad := map[string]store.ReplicaPeers{"a": assign["a"], "gone": assign["b"]}
		err := r.ApplyReplicaPeersBatch(bad, func(map[string]store.ReplicaPeers) error { called = true; return nil })
		if !errors.Is(err, ErrReplicaMemberMissing) {
			t.Fatalf("err = %v", err)
		}
		if called || declared(r, "a") != nil {
			t.Error("a batch naming a missing node must change nothing, store included")
		}
	})
}

// TestReplicaLab_ConcurrentPollCorrelateApplyRemove runs every writer and
// reader of replica state at once; the point is the race detector.
func TestReplicaLab_ConcurrentPollCorrelateApplyRemove(t *testing.T) {
	r, _, _ := twoHostLab(t)
	assign := map[string]store.ReplicaPeers{
		"head":   {Members: []string{"head", "worker"}, Head: "head"},
		"worker": {Members: []string{"head", "worker"}, Head: "head"},
	}
	var wg sync.WaitGroup
	run := func(n int, f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				f(i)
			}
		}()
	}
	run(20, func(int) { r.pollAgentHosts() })
	run(50, func(int) { r.ReplicaSuggestions() })
	run(30, func(int) {
		_ = r.ApplyReplicaPeersBatch(assign, func(map[string]store.ReplicaPeers) error { return nil })
	})
	run(30, func(int) {
		r.RecordHostEvidence(HostEvidence{Host: "extra", Addrs: []string{"10.9.9.9"}})
		r.DropHostEvidence("extra")
	})
	run(10, func(i int) {
		if i%2 == 0 {
			r.RemoveNode("worker")
		} else {
			r.AddNode(config.NodeConfig{Name: "worker", URL: "http://localhost:8000", Runtime: "vllm"})
		}
	})
	wg.Wait()
}
